# Dependency proxy and read-only dependency cache

Repositories whose builds download dependencies (Maven, npm) can be verified
without letting candidate code reach the network. Recurring dependency work
runs as reviewed, non-generative Tekton Tasks; candidate code only ever reads a
cache.

| Component | Where | Network |
|---|---|---|
| Dependency proxy (Maven and npm) | `ai-sdlc-dependencies`, from the chart (`--dependency-proxy`) | The only pod with egress: DNS and HTTPS to the configured upstream hosts |
| Dependency tasks (`maven-dependencies`, `npm-lock-update`) | `ai-sdlc-ci`, from the [Tekton CI](tekton-ci.md) catalog (`--tekton-ci`) | The proxy only |
| Test tasks and ai-sdlc verification pods | `ai-sdlc-ci` (tests), `ai-sdlc-verifiers` (verification) | None (deny-all; verification also `network=none` in the sandbox) |

## 1. The proxy

```bash
./install.sh local ... --dependency-proxy maven,npm
```

`--dependency-proxy` takes `maven`, `npm` or `maven,npm` with `plan` and
`local`. Omitting it on a later install removes the proxy and its cache claim.
It renders the chart's `dependencyProxy` values:

| Value | Default | Meaning |
|---|---|---|
| `image` | `nginxinc/nginx-unprivileged@sha256:0c79…` | The proxy, pinned by digest |
| `clientNamespace` | `ai-sdlc-ci` | Namespace of the dependency tasks |
| `maven.upstreamHost` | `repo.maven.apache.org` | Maven requests go to `https://<host>/maven2/` |
| `maven.allowedPaths` | `[]` | Optional allowlist of repository path prefixes (`io/quarkus/`) |
| `npm.upstreamHost` | `registry.npmjs.org` | npm requests go to `https://<host>/` |
| `npm.allowedPackages` | `[]` | Optional allowlist of package names and scopes (`react`, `@types/*`) |
| `npm.audit` | `false` | Pass `npm audit` advisory requests (POST, never cached) upstream |
| `storageSize`, `storageClass` | `20Gi`, default class | The proxy cache claim |

The chart creates:
- the `ai-sdlc-dependencies` namespace (restricted Pod Security) with a
  `deny-all` policy;
- the `dependency-proxy` Deployment, ConfigMap, Service and cache claim
  (`dependency-proxy-cache`);
- the `dependency-proxy` policy: ingress only from pods of `clientNamespace`
  labelled `ai-sdlc.io/ci-egress: dependency-proxy`, on the enabled ports;
  egress only to cluster DNS and to TCP 443 on public addresses.

The proxy listens on:
- `8080`: Maven, path `/maven2/`;
- `8081`: npm, path `/`.

Tasks use the Service's cluster IP, for example
`http://<cluster IP>:8080/maven2` and `http://<cluster IP>:8081/`, so they need
no DNS egress.

nginx serves only these routes:
- `GET` requests, except the two npm audit endpoints when `npm.audit` is set;
- the configured upstream host only, with certificate verification (TLS SNI
  and name check); the host is re-resolved through the cluster DNS (`resolver`
  rendered from the pod's `/etc/resolv.conf`), so the proxy starts without
  DNS and follows CDN address changes;
- released artifacts and npm tarballs, cached as immutable (ten years);
- `maven-metadata.xml` and npm packuments, cached for ten minutes; a `404`
  for one minute;
- anything else, including `/-/` npm endpoints such as `whoami`, gets `403`.

**Requests.** A request is matched, cached and forwarded by the same
normalized path (`$uri`). A query string, or a path that after decoding
leaves the conservative character set, is refused with `403`, so an encoded
separator (`%2F`) or dot segment cannot make the forwarded path differ from
the allowlisted one. A scoped npm packument (`/@scope%2fname`) is forwarded as
`/@scope/name`.

**Headers and bodies.** No client header is forwarded
(`proxy_pass_request_headers off`), and every proxying location sets only
`Host`, an empty `Authorization`, `Cookie` and `Accept-Encoding`, a fixed
`User-Agent` and, for npm, `Accept` (which is part of the packument cache
key). No request body is forwarded except to the npm audit endpoints. The
cached response is therefore the same for every client.

**Cache size.** `max_size` is nine tenths of `storageSize`, leaving room for
temporary files and the file system.

### The CI side of the contract

The Tekton CI catalog owns the `ai-sdlc-ci` namespace. Its dependency tasks
reach the proxy through this policy, which the catalog ships next to its
default deny (`runtime/k8s/deploy/tekton-ci/ci/network-policy.yaml`):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: ai-sdlc-ci-dependency-proxy, namespace: ai-sdlc-ci}
spec:
  podSelector:
    matchLabels: {ai-sdlc.io/ci-egress: dependency-proxy}
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector:
            matchLabels: {kubernetes.io/metadata.name: ai-sdlc-dependencies}
          podSelector:
            matchLabels: {app.kubernetes.io/component: dependency-proxy}
      ports: [{protocol: TCP, port: 8080}, {protocol: TCP, port: 8081}]
```

The dependency tasks pass the proxy as `maven-mirror-url`
(`maven-dependencies`, `mirrorOf *`, strict checksums) or `registry-url`
(`npm-lock-update`, `--ignore-scripts`). With the proxy they carry
`ai-sdlc.io/ci-egress: dependency-proxy` instead of the public-HTTPS label.
Test tasks carry no egress label, so only the default deny applies to them.

## 2. Offline test phase

A test run mounts the cache read-only and never writes it:

- **Maven 3.9.** Use
  `mvn -o -C -Dmaven.repo.local=/tmp/m2 -Dmaven.repo.local.tail=<cache>`.
  The tail is read-only, and new files would go to the pod-local head. In
  the measured run below nothing was written to the head (0 bytes).
- **npm.** Use `npm ci --offline --ignore-scripts --cache <cache>`. npm
  reads a read-only cache without writing to it.

## 3. ai-sdlc verification with a sealed cache

ai-sdlc's own verifier (`repository-tests/v1`: baseline, cross and candidate
runs) can mount the same kind of cache. Add to the project manifest:

```json
"dependencies": {
  "cachePath": "/deps",
  "keyGlobs": ["**/pom.xml", ".mvn/**", "mvnw", "mvnw.cmd"]
}
```

See [`examples/quarkus-super-heroes-rest-villains.project.json`](../../examples/quarkus-super-heroes-rest-villains.project.json).

The section is omitted when absent: existing manifests keep their digest,
verifier settings and rendered pods.

**Rules:**
- `cachePath` is an absolute path. It cannot be under `/workspaces`, `/tmp`,
  `/runner`, `/etc`, `/proc`, `/sys`, `/dev`, `/run`, `/var/run`,
  `/var/tmp` or a system binary or library directory.
- `keyGlobs` lists 1–32 globs with the manifest glob syntax.
- Files matching `keyGlobs` are always harness files. A candidate that
  changes one is refused (`harness-changed`), whatever `harnessGlobs` says.

**Key.** The cache is the claim `deps-<key>` in `ai-sdlc-verifiers`. `key` is
the sha256 of this text:

```text
ai-sdlc.dependencies/v1
image <verification.image>
settings <sha256 of the canonical dependencies section>
<sha256 of the file>  <path>        (one line per key file, sorted by path)
```

The key covers the base tree only. The cross tree adds test files, and the
candidate cannot change key files, so all three runs read the same cache.

**Before any pod is created,** the verifier reads (`get` only) the claim. The
claim must:
- be bound;
- carry `ai-sdlc.io/dependency-key: <key>`;
- carry the label `ai-sdlc.io/dependency-state: sealed`;
- have only the `ReadOnlyMany`, `ReadWriteMany` or `ReadWriteOnce` access
  modes (`ReadWriteOncePod` would leave two of the three concurrent runs
  pending).

Otherwise the verification stops with `dependencies-unavailable`, and with
`dependencies-key` when the key cannot be computed. Every run attaches the
claim read-only and mounts it read-only in the `tests` container only. The
chart grants the verifier `persistentvolumeclaims: get` in
`ai-sdlc-verifiers` only when the manifest declares the section.

**Filling and sealing a cache.** For each new base revision whose key files
changed:

```bash
# 1. Name the cache and create the empty work claim in ai-sdlc-ci.
python3 tools/dependency_cache_install.py prepare --context kind-ai-sdlc \
  --project-manifest manifest.json --source ./clone --revision <base SHA> --size 8Gi
# 2. Run maven-dependencies on <base SHA> with the work claim as its
#    writable maven-cache workspace and the proxy as maven-mirror-url.
# 3. Hand the filled volume over to the verifier namespace.
python3 tools/dependency_cache_install.py seal --context kind-ai-sdlc \
  --project-manifest manifest.json --source ./clone --revision <base SHA>
```

The work claim `deps-<key>` is separate from the Tekton CI catalog's
per-repository Maven cache (`ai-sdlc-ci-maven-cache-<first 16 hex digits of
sha256(owner/name)>`). That cache stays in `ai-sdlc-ci` for Tekton test tasks
and is never handed over: a sealed `deps-<key>` volume is keyed to one base
revision's dependency files and becomes read-only in the verifier namespace.

`key` prints the claim name without a cluster. The tool reads key files from
`git archive` of the given revision in a local clone (with LF line endings
and no local `autocrlf`), never from a working tree. The runtime reads the
forge's archive of the same revision, which git archive also produces, so
`.gitattributes` `export-ignore`, `export-subst` and end-of-line rules apply
the same way on both sides. If `seal` and the runtime still disagree (for
example a forge-specific `export-subst` value in a key file), the verifier
stops with `dependencies-unavailable`; compare `key` with the claim names.

**Access mode.** `prepare --access-mode` takes `ReadWriteOnce` (default) or
`ReadWriteMany`. A ReadWriteOnce volume can be attached to one node only: the
three concurrent verification runs then work only on a single-node cluster or
with node-local storage whose volume affinity schedules every run on that
node (for example local-path). On a multi-node cluster with network block
storage they fail with Multi-Attach errors; use a ReadWriteMany class (NFS,
CephFS, a cloud file service).

On a local kind cluster created with `--workers` (see
[kind node resources](kind-node-resources.md#worker-nodes)), local-path
volumes stay ReadWriteOnce and node-bound. The Tekton fill runs on a worker
(Tekton's default pod template), local-path gives the volume a node affinity
for that worker, and the sealed claim keeps it. Every verification run that
mounts the cache is therefore scheduled onto that one worker: the three
concurrent runs of a verification (baseline, cross, candidate) share it, and
the worker's kubelet `maxPods` keeps them within its pid limit; extra pods wait
until one ends. `seal` refuses a volume bound to a node that the
`ai-sdlc-offline` RuntimeClass cannot schedule onto (for example a fill that
ran on the control-plane), because every verification of that key would stay
pending.

`seal` works as follows:
- It refuses a claim with another key, and one that a running pod still uses.
- It retains the volume, deletes the work claim, rebinds the volume
  (`claimRef`) to `ai-sdlc-verifiers/deps-<key>`, and creates that claim
  already labelled `sealed`. After the claim binds it checks the key, the
  label and the volume, and restores the volume's original reclaim policy.
- It records the key and the original policy on the volume before deleting
  the work claim, so a rerun resumes after any interruption (including after
  the `claimRef` patch or a bind timeout).
- It is idempotent once sealed.
- It patches a PersistentVolume, so it needs a cluster-admin kubeconfig.

Nothing in `ai-sdlc-verifiers` can write to a sealed volume. Delete old
`deps-*` claims there when their base revision is gone.

## Measurements (os-santiago/quarkus-super-heroes, rest-villains)

Image `maven:3.9-eclipse-temurin-25` (Maven 3.9.16). Kind and podman, 2026-11-27:

| Step | Result |
|---|---|
| Fill an empty cache through the proxy only (direct egress refused) | 554 MB; `mvn test -Dmaven.test.failure.ignore=true` resolves the Surefire JUnit Platform provider. `test -Dtest=NoSuchTest` and `dependency:go-offline` do not |
| Sealed hand-over to `ai-sdlc-verifiers` | Bound, `sealed`; a write from the test pod is refused |
| Offline test run, read-only tail, PostgreSQL sidecar, deny-all | 75 tests, 1 error (the Playwright UI test downloads a browser), 0 bytes written to the local repository |
| npm (ui-super-heroes webui, 1397 packages) | `npm ci --offline --ignore-scripts` from a read-only cache, then `react-scripts test`: 35/35 passed with no network |
| npm lockfile update through the proxy | `npm update <pkg> --package-lock-only --ignore-scripts` exits 0 and writes no `node_modules` |

## Security analysis

- **Candidate code never has network.** Verification pods are still
  `network=none` (sandbox RuntimeClass) in a namespace with exactly one
  deny-all policy, checked before every effect. Test tasks match only the
  CI default deny.
- **Only reviewed base code runs with network, and only towards the proxy.**
  The proxy is not the open internet, but it is not a data diode either:
  code that runs in a dependency task can still encode data in request paths
  (one GET per artifact-shaped path, which the upstream logs), within the
  path allowlist when one is set. Bodies, query strings and client headers
  are not forwarded. This is why only reviewed base code runs there.
  - A cache fill runs the base revision's build. The Tekton run tool accepts a
    warm run only on the current base-branch head.
  - Key files are harness, so a candidate that changes the build or
    dependency definition is refused before its cache could matter.
- **The cache cannot be poisoned by a candidate.**
  - ai-sdlc runs attach the sealed claim read-only and mount it read-only.
  - Maven writes to a pod-local head, and npm reads the cache only.
  - A sealed volume is never writable from the namespace that holds it.
- **One audited egress point.**
  - The proxy is the namespace's only pod with egress, to DNS and public
    HTTPS.
  - nginx sends requests only to the configured hosts, with TLS
    verification. A request cannot choose the host.
  - Methods are limited to `GET`, and optional allowlists narrow the paths.
- **Integrity.**
  - npm verifies every tarball against the lockfile's `integrity` hash, which
    the reviewed lockfile pins.
  - Maven's strict checksums (`-C`) compare each file with the `.sha1` or
    `.sha512` file served by the same upstream through the same proxy. That
    detects corruption in transit or in the cache, not a malicious upstream,
    and nothing pins Maven artifacts the way a lockfile does.
  - Cached artifacts are immutable, so a later upstream change is not
    served.
  - The proxy does not verify checksums or PGP signatures itself; neither do
    Maven or npm verify signatures by default.
- **No credentials anywhere.** The proxy uses anonymous upstream access.
  The dependency tasks and the seal tool use no tokens. `seal` needs an
  administrator's kubeconfig, used locally.
- **Disclosure.** Upstream registries see which artifacts the installation
  downloads. With `npm.audit`, the npm registry also receives the audited
  lockfile's package names and versions.

## Dependency updates (for example vulnerability fixes)

A model never hand-edits a lockfile: integrity hashes make that impossible to
review and easy to get wrong. The flow is:

1. **The trusted update step: `npm-lock-update`.** It is a Tekton Task, not
   a generator tool. The adapter starts it after generation or instead of it
   for a dependency-only issue.
   - It runs `npm update <names> --package-lock-only --ignore-scripts` or
     `npm audit fix --package-lock-only --ignore-scripts`, through the proxy
     only.
   - No package code runs, and no `node_modules` is written.
   - Its inputs from the issue are package names (and optional exact
     versions). Each name is validated against the base lockfile.
   - It publishes the bounded change and the diff digest. The adapter makes
     exactly that diff (`package.json`, `package-lock.json`) the candidate.
   - For Maven, the equivalent is a `<version>`-only change to a `pom.xml`.
2. **Verification of such a candidate (not implemented yet).** Its key
   files differ from the base, so the base cache does not contain the new
   versions. A safe design:
   - Accept a key-file change only when it is byte-identical to the
     recorded output of the trusted update step (lockfile digest), or for
     Maven when it changes only `<version>` text of existing dependencies.
   - Fill a candidate-scoped cache with resolution only:
     - npm: `npm ci --ignore-scripts` downloads and integrity-checks exactly
       the lockfile's tarballs and runs no package code.
     - Maven: resolve declared dependencies only, with no new plugins.
   - Seal it like a base cache, and run the three offline runs against it.
   - Any other change to a key file stays `harness-changed`.

   Until this exists, dependency-update candidates are refused by ai-sdlc's
   verifier (`harness-changed`) and are verified by the Tekton test tasks
   instead.

## Maintainer decisions

1. **nginx instead of Sonatype Nexus.** nginx (about 55 MB, 64 MiB of
   memory) serves both ecosystems with a static, reviewed configuration and
   no credentials. Nexus needs about 2–4 GiB of memory. Creating its npm
   proxy repository needs the administrator password through its REST API.
   Recent Nexus Community Edition releases also have usage limits. Nexus
   adds a UI, component search, repository health checks and per-repository
   policies. If those are wanted, replace the Deployment behind the same
   Service, ports and labels.
2. **The hand-over needs a cluster-scoped privilege** (patching a
   PersistentVolume), so `seal` is an operator step today. Automating it as a
   Tekton Task means giving a CI ServiceAccount `persistentvolumes: patch`,
   restricted by a ValidatingAdmissionPolicy to volumes whose `claimRef` is a
   `deps-*` claim in `ai-sdlc-ci`. ReadWriteMany storage, or verification
   moving into Tekton test tasks, would remove the hand-over altogether.
3. **DNS-free tasks.** Dependency tasks get the proxy's cluster IP, so they
   need no DNS egress (DNS is a covert channel). The adapter must resolve the
   Service before creating the run.
4. **`npm.audit` defaults to off,** because it discloses the dependency set
   to the registry. Enable it for `npm audit fix`.
5. **The Playwright UI test in rest-villains** downloads a browser at run
   time. Exclude it offline or use an image with the browser.

## Remaining work

- Switching `maven-dependencies`, `npm-lock-update` and `dependency-audit`
  from the `fetch` label to `ai-sdlc.io/ci-egress: dependency-proxy` when the
  proxy is installed (the catalog already ships the policy above).
- Automatic cache fill and seal per base revision (Tekton trigger on
  base-branch pushes that change key files), and pruning of old `deps-*`
  claims.
- Verification of dependency-update candidates (section above).
- An npm shared cache claim for Tekton test tasks, filled by
  `npm ci --ignore-scripts` on the base revision.
