# Database backup and restore

The runtime keeps its durable state in one PostgreSQL database: the intake
journal, the work queues, the execution and review records, and the
transactional outbox. The chart can back this database up on a schedule. The
backup is off by default. This page explains how to turn it on, check a
backup, restore it into a fresh installation, and test the restore.

## Names used on this page

The chart names its backup objects `<fullname>-backup`. `<fullname>` is the
chart's full name: `fullnameOverride` when set, otherwise derived from the
release name. The local installer sets `fullnameOverride: ai-sdlc` and the
namespace `ai-sdlc-runtime`, so the examples use `ai-sdlc-backup` and
`ai-sdlc-runtime`. These names hold only with `fullnameOverride: ai-sdlc`.
Find yours with:

```bash
kubectl -n <namespace> get cronjob -l app.kubernetes.io/component=backup
```

The examples also select the release's pods with
`app.kubernetes.io/instance=ai-sdlc`. Use your release name there.

## How the backup works

With `backup.enabled`, the chart renders three objects:

- A CronJob, `<fullname>-backup`. Each run calls `pg_dump --format=custom`
  and writes one file, `ai-sdlc-<UTC time>.dump`, to the backup volume, with
  a `.sha256` checksum file next to it.
- A PersistentVolumeClaim, `<fullname>-backup`, unless you name an existing
  one. Helm keeps it when you uninstall the release.
- A NetworkPolicy, `<fullname>-backup`, with `networkPolicy.enabled`. The
  backup pod accepts no traffic. It can reach only DNS and the database.

Each run is safe to interrupt:

1. It takes a lock on the volume (a `.ai-sdlc-backup.lock` directory). If
   another backup or restore holds it, the run fails and touches nothing. A
   lock older than `backup.activeDeadlineSeconds` was left by a killed run,
   because no run outlives that deadline; the next run removes it.
2. It removes temporary files a killed run left behind, then writes the dump
   to a new hidden temporary file.
3. It reads the whole new file with `pg_restore --file=/dev/null`. This
   reads every data block, so a truncated or corrupt file fails and is
   deleted. (`pg_restore --list` is not enough: it reads only the table of
   contents at the start of the file.)
4. It writes the file's SHA-256 checksum, then renames the file to its final
   name. A file with the final name is always complete.
5. It deletes the oldest backups, so that only the newest
   `backup.retention` files stay. It never deletes the file it just wrote,
   even if the clock went back, and never deletes other files. A failed run
   deletes nothing.

The backup connects as the database's bootstrap owner, the same role the
schema migrator uses. It reads the owner's URL from the database Secret
(`postgres.databaseURLSecret`, or `<clusterName>-app` with CloudNativePG)
through a `secretKeyRef`. No secret value goes into Helm values. The script
moves the password from the URL into `PGPASSWORD`, so it never appears on a
command line or in the log.

The pod runs with a restricted security context: non-root, a read-only root
filesystem with an `emptyDir` for `/tmp`, all capabilities dropped, the
`RuntimeDefault` seccomp profile, and no service account token. It uses the
namespace's `default` ServiceAccount, which needs no permission. Outside
OpenShift it runs as UID and GID 70, the PostgreSQL image's `postgres` user.
With `openshift.enabled` the chart sets no UID, so the `restricted-v2` SCC
assigns one from the namespace range.

Backup files have mode `0640`: the writing user can read and write them, and
the volume's group (the pod's `fsGroup`) can read them. Other users cannot.
Group read lets a pod that runs with the same `fsGroup` but another UID, such
as the inspect pod below, read a backup without being able to change it.

## Enable the backup

Add a `backup` block to your values and upgrade the release:

```yaml
backup:
  enabled: true
  schedule: "17 2 * * *"   # daily at 02:17
  timeZone: Etc/UTC
  retention: 7             # keep the newest seven files
  storage:
    existingClaim: ""      # or the name of a claim you created
    size: 5Gi
    storageClass: ""       # empty uses the cluster default
```

Other settings, with their defaults:

| Value | Default | Meaning |
|---|---|---|
| `backup.image` | `postgres.local.image` | The `pg_dump` image. It must be pinned by digest. |
| `backup.startingDeadlineSeconds` | `600` | A run that cannot start this long after its schedule is skipped. |
| `backup.backoffLimit` | `1` | Retries of a failed run. |
| `backup.activeDeadlineSeconds` | `3600` | Deadline of one run, retries included. Also the age of an abandoned lock. |
| `backup.resources` | 50m/64Mi requested, 1 CPU/512Mi limit | Container resources. |

The CronJob uses `concurrencyPolicy: Forbid`, so it never starts a scheduled
run while one is active. `Forbid` does not cover a Job you create by hand
from the CronJob; the volume lock does.

Size the volume for `retention` dumps plus one. A custom-format dump is
compressed and is usually much smaller than the database.

The volume is in the same cluster as the database. It does not protect
against losing the cluster or its storage. Copy backups somewhere else on a
schedule (see [Verify a backup](#verify-a-backup)).

### Bundled, CloudNativePG and external databases

The backup always targets the database the components use:

- **Bundled PostgreSQL** (`postgres.local.enabled`). The default image is the
  server's own image, so `pg_dump` has the same major version. The
  NetworkPolicy allows the bundled PostgreSQL pod on port 5432.
- **CloudNativePG.** The backup reads `uri` from the operator's
  `<clusterName>-app` Secret. Set `backup.image` to a PostgreSQL image with
  the cluster's major version.
- **External PostgreSQL.** The backup reads `postgres.databaseURLSecret`,
  like the migrator. Set `backup.image` to an image whose `pg_dump` major
  version is the same as the server's or newer.

With CloudNativePG or an external server, the NetworkPolicy allows
`networkPolicy.databaseEgressCIDRs` on port 5432, as for the components. Set
it to the database address. The port is fixed: the chart's policies open
only 5432, so an external server on another port is not reachable with
`networkPolicy.enabled`.

### The owner URL

The URL has the form `postgresql://<user>:<password>@<host>[:<port>]/<database>[?<parameters>]`.
The script splits it as follows:

- The password is percent-decoded (RFC 3986), so any character works when
  encoded: `/` as `%2F`, `@` as `%40`, `:` as `%3A`, `%` as `%25`, `?` as
  `%3F`, `#` as `%23`, a space as `%20`. Encoding is always safe.
- Raw `@`, `:`, `?` and `#` in the password also work.
- A raw `/` in the user name or password is refused. It would end the host
  part early and leave the password in the URL.
- An invalid escape (such as `%2` or `%zz`) and an encoded NUL (`%00`) are
  refused.
- A `password=` query parameter is refused. Put the password in the URL's
  user part instead.
- The user name, host, port, database and query parameters go to the
  PostgreSQL tools unchanged. Encode reserved characters in the user name.

A refusal names the problem but never prints any part of the URL. The
installer's and CloudNativePG's generated passwords need no encoding.

TLS: the script passes the URL's query parameters to `pg_dump` unchanged.
`sslmode=require` encrypts the connection without checking the server's
certificate. `verify-ca` and `verify-full` need a CA file, and the chart
does not mount one. Verified TLS to an external database is not covered yet
(audit gap 5.5, see [Helm chart installation](helm-chart-install.md)).

### Roles and row-level security

The tables use row-level security, and most of them force it on their owner
too. `pg_dump` and `pg_restore` turn row security off for their session, so
the backup role must be a superuser or have `BYPASSRLS`. The migrator already
requires this of the bootstrap owner. If the role lacks it, `pg_dump` fails
and no file is written. It never writes a partial dump.

## Check that backups run

```bash
kubectl -n ai-sdlc-runtime get cronjob ai-sdlc-backup
kubectl -n ai-sdlc-runtime get jobs -l app.kubernetes.io/component=backup
kubectl -n ai-sdlc-runtime logs job/<job name>
```

A successful run logs `backup: wrote <file> (<n> bytes)` and `backup: done`.
To take a backup now, start a Job from the CronJob:

```bash
kubectl -n ai-sdlc-runtime create job ai-sdlc-backup-manual --from=cronjob/ai-sdlc-backup
```

## Verify a backup

Start a short-lived pod that mounts the backup volume read-only. It uses the
CronJob's image and needs no database access:

```bash
image="$(kubectl -n ai-sdlc-runtime get cronjob ai-sdlc-backup \
  -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}')"
kubectl -n ai-sdlc-runtime apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ai-sdlc-backup-inspect
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 70        # omit the three IDs on OpenShift
    runAsGroup: 70
    fsGroup: 70
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: inspect
      image: ${image}
      command: [sleep, "3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: [ALL]}
        readOnlyRootFilesystem: true
      volumeMounts: [{name: backup, mountPath: /backup, readOnly: true}]
  volumes:
    - name: backup
      persistentVolumeClaim: {claimName: ai-sdlc-backup, readOnly: true}
EOF
kubectl -n ai-sdlc-runtime wait --for=condition=Ready pod/ai-sdlc-backup-inspect
```

The claim is `ReadWriteOnce`. If the pod stays pending, the volume is
attached to another node; wait for the running backup Job to finish.

List the backups, check one file's checksum, read the whole file, then list
its table of contents:

```bash
file=ai-sdlc-20261125T021700Z.dump
kubectl -n ai-sdlc-runtime exec ai-sdlc-backup-inspect -- ls -l /backup
kubectl -n ai-sdlc-runtime exec ai-sdlc-backup-inspect -- sh -c "cd /backup && sha256sum -c $file.sha256"
kubectl -n ai-sdlc-runtime exec ai-sdlc-backup-inspect -- pg_restore --file=/dev/null "/backup/$file"
kubectl -n ai-sdlc-runtime exec ai-sdlc-backup-inspect -- \
  pg_restore --list "/backup/$file" | grep -E 'TABLE DATA|POLICY|ROW SECURITY' | head
```

`pg_restore --file=/dev/null` reads every data block and fails on a truncated
or corrupt file. `pg_restore --list` reads only the table of contents, so it
passes on a file cut short; use it to see what the archive holds, not to
prove it is complete. A good archive lists one `TABLE DATA` entry per table,
plus the `ROW SECURITY` and `POLICY` entries.

Copy a backup and its checksum off the cluster, then delete the pod:

```bash
kubectl -n ai-sdlc-runtime exec ai-sdlc-backup-inspect -- cat "/backup/$file" > "$file"
kubectl -n ai-sdlc-runtime exec ai-sdlc-backup-inspect -- cat "/backup/$file.sha256" > "$file.sha256"
sha256sum -c "$file.sha256"
kubectl -n ai-sdlc-runtime delete pod ai-sdlc-backup-inspect
```

A dump holds every workflow record. Store copies with the same care as the
database.

## Restore into a fresh installation

A restore replaces every table, row, privilege and policy in the database
with the archive's contents. It runs as one transaction: it applies
completely or not at all.

Restore into the chart version that took the backup. Upgrade afterwards, as
usual; the migrator then applies the newer migrations. Restoring an archive
into a newer installation's schema, or into an older chart, is not supported.

1. **Install.** Install the same chart version with the same values, with
   `backup.enabled` set. Use the original installation's authority policy and
   identity Secrets if you still have them (see
   [What is not backed up](#what-is-not-backed-up)). Wait for the migrator Job
   to complete. It creates the schema and one login role per component.

2. **Stop the runtime.** Record each Deployment's replica count, scale every
   component to zero, and pause the backup schedule:

   ```bash
   kubectl -n ai-sdlc-runtime get deployment -l app.kubernetes.io/instance=ai-sdlc \
     -o jsonpath='{range .items[*]}{.metadata.name}={.spec.replicas}{"\n"}{end}' > replicas.txt
   kubectl -n ai-sdlc-runtime scale deployment -l app.kubernetes.io/instance=ai-sdlc --replicas=0
   kubectl -n ai-sdlc-runtime patch cronjob ai-sdlc-backup -p '{"spec":{"suspend":true}}'
   kubectl -n ai-sdlc-runtime wait --for=delete pod --timeout=300s \
     -l 'app.kubernetes.io/instance=ai-sdlc,app.kubernetes.io/component notin (postgres,migrator,backup)'
   ```

   `replicas.txt` keeps the counts the chart rendered. Some are 0 on
   purpose, for example the intake with `runtime.intakePaused` or every
   component with `runtime.paused`.

   PostgreSQL keeps running. The restore refuses to start while any component
   role (`aisdlc_*`) is still connected.

3. **Put the file on the backup volume.** Skip this step if the new
   installation uses the old backup claim (`backup.storage.existingClaim`).
   Otherwise create the inspect pod above without `readOnly` (in both places)
   and copy the file and its checksum in. Keep their names:

   ```bash
   kubectl -n ai-sdlc-runtime exec -i ai-sdlc-backup-inspect -- sh -c "cat > /backup/$file" < "$file"
   kubectl -n ai-sdlc-runtime exec -i ai-sdlc-backup-inspect -- sh -c "cat > /backup/$file.sha256" < "$file.sha256"
   kubectl -n ai-sdlc-runtime delete pod ai-sdlc-backup-inspect
   ```

4. **Restore.** Create a Job from the CronJob and give it `RESTORE_FILE`. The
   Job runs the same script, with the same Secret reference, security context
   and network policy. With `RESTORE_FILE` set it restores instead of taking
   a backup:

   ```bash
   kubectl -n ai-sdlc-runtime create job ai-sdlc-restore --from=cronjob/ai-sdlc-backup \
       --dry-run=client -o yaml \
     | kubectl set env --local -f - RESTORE_FILE="$file" -o yaml \
     | kubectl -n ai-sdlc-runtime apply -f -
   kubectl -n ai-sdlc-runtime logs -f job/ai-sdlc-restore
   ```

   The Job takes the volume lock, checks the file against its checksum when
   one is present, reads the whole file with `pg_restore --file=/dev/null`,
   and checks that no component is connected. Then it runs
   `pg_restore --clean --if-exists --no-owner --single-transaction --exit-on-error`.
   Success logs `backup: restored <file>`. On failure the transaction rolls
   back and the database keeps its previous contents.

5. **Start the runtime.** Delete the Job, give each Deployment back its
   recorded replica count, and resume the schedule:

   ```bash
   kubectl -n ai-sdlc-runtime delete job ai-sdlc-restore
   while IFS='=' read -r name count; do
     kubectl -n ai-sdlc-runtime scale deployment "$name" --replicas="$count"
   done < replicas.txt
   kubectl -n ai-sdlc-runtime patch cronjob ai-sdlc-backup -p '{"spec":{"suspend":false}}'
   ```

   Do not scale every Deployment to 1: that would start an intake the
   values pause.

### What restore does with roles and row-level security

- **Roles.** Login roles belong to the PostgreSQL server, not to one
  database. A dump never contains them or their passwords. The new
  installation's migrator created the component roles, with the passwords in
  the new installation's Secrets. The restore keeps those roles unchanged, so
  the components connect with their current Secrets.
- **Privileges.** The archive holds each role's table and column grants.
  The restore applies them by role name, so each component gets exactly the
  access it had at backup time.
- **Row-level security.** The archive holds each table's `ENABLE` and
  `FORCE ROW LEVEL SECURITY` flags and every policy. `pg_restore` loads the
  rows first and applies the flags and policies after. The restoring role
  needs superuser or `BYPASSRLS`, as for the backup.
- **Ownership.** `--no-owner` makes the restoring role, the bootstrap owner,
  the owner of every restored object, as the migrator would.

## What is not backed up

The backup holds only the runtime database. It does not hold:

- **Forge state.** Issues, comments, branches and pull requests live on the
  forge (GitHub). The restored journal does not know about anything done on
  the forge after the backup, such as a pull request the runtime opened or
  merged.
- **Images and registry content.** Runtime images, built candidate images
  and registry contents are not backed up. Keep the image lock and use
  digest-pinned images, so a reinstall pulls the same bytes.
- **Kubernetes objects.** Secrets, ConfigMaps, Helm values, the authority
  policy and the identity Secrets are not in the dump. Keep a secured copy
  of the values and of the Secrets in the
  [Secret contract](helm-chart-secret-contract.md). Restoring with new
  identities or a new authority policy has not been tested.
- **In-flight pods and workspaces.** Sandbox pods, their workspaces and
  model gateway state are not backed up. Work that was running at backup
  time comes back only as its database records. How each loop recovers
  such work after a restore has not been tested.
- **Logs and metrics.**

## Test the restore

Test the backup logic without a cluster:

```bash
python3 -m unittest discover -s tools -p test_helm_chart_backup.py -v
```

These tests render the chart and run the backup script against stub
`pg_dump`, `pg_restore` and `psql` commands. They check:

- the atomic write, the checksum, the file mode and the retention;
- that a truncated archive is refused;
- the volume lock, with concurrent runs and a stale lock;
- the refusal while a component is connected;
- that the password never reaches a command line or the output, for every
  URL form above.

Rehearse a real restore before you need one, at least after each chart
upgrade:

1. Install the same chart version into a scratch namespace or cluster.
2. Copy a recent production backup off the cluster, as above.
3. Follow [Restore into a fresh installation](#restore-into-a-fresh-installation)
   in the scratch installation.
4. Compare the two databases. For example, as the owner, count the rows of
   `schema_migrations` and of the queue tables in both. Then check that the
   components become ready.
5. Delete the scratch installation and its backup claim.

The script was also run outside a cluster, against the bundled PostgreSQL
image, with a table that forces row-level security:

- Three backups with retention 2 kept two files, each with a valid checksum.
- The same backup and restore worked with `/@:%?#` in the password, sent
  percent-encoded in the URL.
- A copy of a backup cut to half its size passed `pg_restore --list` and
  failed `pg_restore --file=/dev/null`. A restore of it was refused.
- A restore into a fresh database with the role pre-created was refused
  while that role was connected. After the session ended, the restore brought
  back every row, the forced row-level security, the policy and the grant.
  The role then saw only its own rows.

A restore on a live cluster has not been recorded yet.
