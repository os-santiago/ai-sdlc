"""Candidate-only installation inputs; never emits a workflow command."""
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import secrets

# Generation budget bounds selectable at installation. The defaults are the
# reviewed profile; the maxima stay within the runtime's own limits (32 steps,
# 900 s) and the lifecycle's 1,020 s ceiling (maxSeconds + 120 s).
DEFAULT_GENERATION_SECONDS, MIN_GENERATION_SECONDS, MAX_GENERATION_SECONDS = 600, 600, 900
DEFAULT_GENERATION_STEPS, MIN_GENERATION_STEPS, MAX_GENERATION_STEPS = 24, 24, 32
# Requested output tokens per model answer (the profile's maxTokens). The
# maximum is the runtime's own cap: executor profile validation, the mailbox
# RequestLimits and the bundled adapter contract all refuse more than 8192.
DEFAULT_ANSWER_TOKENS, MIN_ANSWER_TOKENS, MAX_ANSWER_TOKENS = 4096, 4096, 8192


def generation_budget(seconds=None, steps=None, model_upstream=False):
    """Validated (seconds, steps); None selects the reviewed default.

    More than 24 steps requires a model upstream, whose call ledger follows
    the steps. The bundled Codex adapter keeps its reviewed 16-call ledger, so
    extra tool steps there would rarely be reachable and only look larger."""
    seconds = DEFAULT_GENERATION_SECONDS if seconds is None else seconds
    steps = DEFAULT_GENERATION_STEPS if steps is None else steps
    if type(seconds) is not int or not MIN_GENERATION_SECONDS <= seconds <= MAX_GENERATION_SECONDS:
        raise ValueError('Generation seconds must be an integer from 600 to 900')
    if type(steps) is not int or not MIN_GENERATION_STEPS <= steps <= MAX_GENERATION_STEPS:
        raise ValueError('Generation steps must be an integer from 24 to 32')
    if steps > DEFAULT_GENERATION_STEPS and model_upstream is not True:
        raise ValueError('--generation-steps above 24 requires --model-upstream: the bundled Codex adapter keeps '
                         'its 16-call provider ledger, which a larger step budget would not raise')
    return seconds, steps


def answer_tokens(tokens=None, model_upstream=False):
    """Validated per-answer token bound; None selects the reviewed default.

    The bound should fit a whole-file rewrite of a publishable file (up to
    32 KiB, about 10k tokens of code): a model whose patches are refused falls
    back to rewriting files, and a truncated answer stops the attempt. Above the
    default requires a model upstream: the bundled Codex adapter validates
    max_tokens but never applies it to the Codex CLI, so a larger bound there
    would change the reviewed configuration without changing any answer."""
    tokens = DEFAULT_ANSWER_TOKENS if tokens is None else tokens
    if type(tokens) is not int or not MIN_ANSWER_TOKENS <= tokens <= MAX_ANSWER_TOKENS:
        raise ValueError('Generation answer tokens must be an integer from 4096 to 8192')
    if tokens > DEFAULT_ANSWER_TOKENS and model_upstream is not True:
        raise ValueError('--generation-answer-tokens above 4096 requires --model-upstream: the bundled Codex adapter '
                         'does not apply a per-answer token bound, so a larger one would change nothing')
    return tokens


def installed_execution_config(kubectl):
    """The installed execution configuration document, or None before the first."""
    import install
    result = install.run(kubectl + ['--request-timeout=10s', 'get', 'configmap', 'ai-sdlc-execution',
        '-n', install.NAMESPACE, '--ignore-not-found', '-o', 'json'], capture_output=True, timeout=15)
    if not result.stdout.strip():
        return None
    def unique_object(pairs):
        result = dict(pairs)
        if len(result) != len(pairs):
            raise ValueError('Duplicate installed execution configuration field')
        return result
    try:
        if len(result.stdout.encode()) > 1048576:
            raise ValueError()
        installed = json.loads(result.stdout, object_pairs_hook=unique_object)
        raw = installed['data']['config.json']
        if not isinstance(raw, str) or len(raw.encode()) > 262144:
            raise ValueError()
        config = json.loads(raw, object_pairs_hook=unique_object)
        if not isinstance(config, dict):
            raise ValueError()
    except (KeyError, TypeError, ValueError):
        raise ValueError('Installed execution configuration is malformed; preserve it for review') from None
    return config


def _field(document, name, default):
    """A field matched case-insensitively; more than one spelling is malformed."""
    values = [value for key, value in document.items() if key.lower() == name.lower()]
    if len(values) > 1:
        raise ValueError('Installed execution configuration is malformed; preserve it for review')
    return values[0] if values else default


def require_runtime_class(kubectl, explicit):
    """A reinstall keeps the installed sandbox RuntimeClass unless the operator
    names a class explicitly; omitting --runtime-class never silently switches
    an installation back to ai-sdlc-offline."""
    config = installed_execution_config(kubectl)
    if config is None:
        return
    # The runtime matches JSON field names case-insensitively; so does this guard.
    placement = _field(config, 'placement', {})
    installed = _field(placement, 'RuntimeClass', DEFAULT_RUNTIME_CLASS) if isinstance(placement, dict) else None
    if not isinstance(installed, str):
        raise ValueError('Installed execution configuration is malformed; preserve it for review')
    if explicit is None and installed != DEFAULT_RUNTIME_CLASS:
        raise ValueError('The installation uses sandbox RuntimeClass ' + validate_runtime_class(installed)
                         + '; pass --runtime-class ' + installed + ' to keep it, or another name to change it explicitly')


def require_unchanged_tool_budget(kubectl, desired):
    """Ordinary reconciliation cannot silently change an installed SCC budget."""
    config = installed_execution_config(kubectl)
    if config is None:
        return  # First candidate configuration, including maintenance promotion.
    try:
        profile = config['profile']
        steps, seconds, tokens = profile['maxSteps'], profile['maxSeconds'], profile['maxTokens']
        if (config['version'] != 'ai-sdlc.candidate-runtime/v1'
                or profile['version'] != 'ai-sdlc.executor-profile/v1'
                or type(steps) is not int or not 1 <= steps <= 32
                or type(seconds) is not int or not 1 <= seconds <= 900
                or type(tokens) is not int or not 1 <= tokens <= MAX_ANSWER_TOKENS):
            raise ValueError()
    except (KeyError, TypeError, ValueError):
        raise ValueError('Installed execution configuration is malformed; preserve it for review') from None
    expected = json.loads(desired['candidate']['configJSON'])['profile']
    if steps != expected['maxSteps']:
        raise ValueError('Installed SCC tool budget differs; repeat the recorded --generation-steps. No runtime transition '
                         'changes a selected budget: changing it requires a reinstallation (the reviewed 16-to-24-step '
                         'transition applies only to its original source)')
    # The recorded generation time is reselected with the same
    # --generation-seconds; omitting it never resets an installed budget.
    if seconds != expected['maxSeconds']:
        raise ValueError('Installed generation time budget differs; repeat the recorded --generation-seconds. No runtime '
                         'transition changes it: changing an installed budget requires a reinstallation')
    # The per-call window exists exactly when a model upstream is installed, so
    # reconciliation can neither switch the model service nor change its window.
    if profile.get('callSeconds') != json.loads(desired['candidate']['configJSON'])['profile'].get('callSeconds'):
        raise ValueError('Installed model service differs; use a reviewed runtime transition before reconciliation')
    # The recorded answer bound is reselected with the same
    # --generation-answer-tokens; omitting it never resets an installed bound.
    if tokens != expected['maxTokens']:
        raise ValueError('Installed generation answer-token bound differs; repeat the recorded --generation-answer-tokens. '
                         'No runtime transition changes it: changing an installed bound requires a reinstallation')
    if config.get('model', {}).get('Limits') != json.loads(desired['candidate']['configJSON']).get('model', {}).get('Limits'):
        raise ValueError('Installed model budget differs; use a reviewed runtime transition before reconciliation')


DEFAULT_RUNTIME_CLASS = 'ai-sdlc-offline'
_RUNTIME_CLASS = re.compile(r'[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')


def validate_runtime_class(name):
    """A Kubernetes RuntimeClass name (DNS-1123 subdomain); never empty."""
    if not isinstance(name, str) or len(name) > 253 or not _RUNTIME_CLASS.fullmatch(name):
        raise ValueError('Invalid sandbox RuntimeClass name; use a DNS-1123 subdomain such as gvisor or kata')
    return name


def desired_values(root, image_lock_path, lock, verification=False, review=False, publication=False, merge=False, repair=False,
                   generation_seconds=None, generation_steps=None, model_upstream=False, runtime_class=DEFAULT_RUNTIME_CLASS,
                   generation_answer_tokens=None):
    seconds, steps = generation_budget(generation_seconds, generation_steps, model_upstream)
    tokens = answer_tokens(generation_answer_tokens, model_upstream)
    if type(repair) is not bool or (repair and not review):
        raise ValueError("Review repair requires explicit independent review")
    if merge and not publication:
        raise ValueError('Merge requires verified publication')
    if publication and (not review or not verification):
        raise ValueError('Publication requires verification and independent review')
    if review and not verification:
        raise ValueError('Independent review requires verification')
    evidence_path = Path(image_lock_path).with_name('images.evidence.json')
    with evidence_path.open('rb') as stream:
        raw = stream.read(65537)
    if len(raw) > 65536:
        raise ValueError('Execution image evidence exceeds bound')
    evidence = json.loads(raw)
    if evidence.get('revision') != lock['image']['tag'] or evidence.get('repository') != 'os-santiago/ai-sdlc':
        raise ValueError('Execution images require matching reviewed publication evidence')
    images = evidence.get('images', {})
    for name, digest in [('worker', lock['components']['worker']['digest']),
                         ('model-mailbox', images.get('model-mailbox', {}).get('digest'))]:
        item = images.get(name, {})
        if (item.get('revision') != lock['image']['tag'] or item.get('repository') != lock['image']['repository']
                or item.get('platform') != 'linux/amd64' or item.get('digest') != digest
                or not isinstance(digest, str) or not re.fullmatch(r'sha256:[a-f0-9]{64}', digest)):
            raise ValueError('Invalid execution image publication identity')
    adev = (root / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml').read_text()
    profile = {'version': 'ai-sdlc.executor-profile/v1', 'model': 'codex-cli',
               # SCC counts tools, including parallel calls, not model turns.
               'maxSteps': steps, 'maxSeconds': seconds, 'maxTokens': tokens,
               'aDevSHA256': hashlib.sha256(adev.encode()).hexdigest()}
    config = {'version': 'ai-sdlc.candidate-runtime/v1', 'profile': profile, 'aDev': adev,
              'placement': {'Namespace': 'ai-sdlc-executors',
                            'WorkerImage': lock['image']['repository'] + '@' + images['worker']['digest'],
                            'MailboxImage': lock['image']['repository'] + '@' + images['model-mailbox']['digest']},
              # The bundled Codex adapter keeps its reviewed provider quota at
              # any step budget: steps count tools, several per model turn.
              # A model upstream derives its calls from steps instead.
              'model': {'Limits': {'Calls': 16, 'InputBytes': 2097152, 'OutputBytes': 4194304},
                        'ResponseLimit': 262144}}
    # The sandbox RuntimeClass is written only when it differs from the default,
    # so default installations keep byte-identical configuration documents.
    custom_runtime_class = validate_runtime_class(runtime_class) != DEFAULT_RUNTIME_CLASS
    if custom_runtime_class:
        config['placement']['RuntimeClass'] = runtime_class
    result = {'candidate': {'configJSON': json.dumps(config, sort_keys=True), 'apiServerCIDRs': []}}
    if custom_runtime_class:
        result['sandbox'] = {'runtimeClassName': runtime_class}
    if verification:
        item = images.get('verifier', {})
        value = item.get('digest')
        if (item.get('component') != 'verifier' or item.get('revision') != lock['image']['tag'] or item.get('repository') != lock['image']['repository']
                or item.get('platform') != 'linux/amd64' or not isinstance(value, str)
                or not re.fullmatch(r'sha256:[a-f0-9]{64}', value)):
            raise ValueError('Verification requires matching reviewed verifier image publication')
        result['verifier'] = {'digest': value}
    if review:
        # Reviewers answer with a short verdict: their answer bound stays the default.
        review_profile = dict(profile, version='ai-sdlc.reviewer-profile/v1', maxSteps=8, maxSeconds=180,
                              maxTokens=DEFAULT_ANSWER_TOKENS)
        review_config = {'version': 'ai-sdlc.independent-review-runtime/v1', 'profile': review_profile,
                         'aDev': adev, 'workerImage': config['placement']['WorkerImage'],
                         'mailboxImage': config['placement']['MailboxImage']}
        if custom_runtime_class:
            review_config['runtimeClass'] = runtime_class
        # The review gateway offers the reviewer model only the read-only tools
        # the review response policy accepts. Installations recorded before
        # this setting omit it and keep rendering exactly as installed.
        result['review'] = {'enabled': True, 'configJSON': json.dumps(review_config, sort_keys=True),
                            'narrowModelTools': True}
    if repair:
        config['profile']['maxRepairs'] = 1
        result['candidate']['configJSON'] = json.dumps(config, sort_keys=True)
    if publication:
        result['publication'] = {'enabled': True}
    if merge:
        result['merge'] = {'enabled': True}
    return result


def model_credentials(kubectl, cluster_uid, auth_file, refresh=False, installer=None):
    # Imported here so the pure desired-state function has no installer effects.
    from install import owned_secret, create_owned_secret, run
    if installer is not None:
        owned_secret, create_owned_secret, run = installer.owned_secret, installer.create_owned_secret, installer.run
    current = owned_secret(kubectl, 'ai-sdlc-codex-auth', cluster_uid)
    if current is None or refresh:
        if auth_file is None:
            raise ValueError('Candidate installation needs --codex-auth-file or an existing owned model Secret')
        with Path(auth_file).open('rb') as stream:
            raw = stream.read(16385)
        if not 0 < len(raw) <= 16384 or not isinstance(json.loads(raw), dict):
            raise ValueError('Invalid model authentication document')
        if current is None:
            create_owned_secret(kubectl, 'ai-sdlc-codex-auth', {'auth.json': raw.decode()}, cluster_uid)
        else:
            # Explicit refresh affects credentials only, not workflow state.
            import subprocess
            run(kubectl + ['patch', 'secret', 'ai-sdlc-codex-auth', '-n', 'ai-sdlc-runtime', '--type=merge', '--patch-file=/dev/stdin'],
                input=json.dumps({'stringData': {'auth.json': raw.decode()}}), stdout=subprocess.DEVNULL)
    elif set(current) != {'auth.json'}:
        raise ValueError('Unexpected model authentication Secret shape')
    client = owned_secret(kubectl, 'ai-sdlc-model-client', cluster_uid)
    if client is None:
        create_owned_secret(kubectl, 'ai-sdlc-model-client', {'token': secrets.token_urlsafe(48)}, cluster_uid)
    elif set(client) != {'token'} or not re.fullmatch(r'[A-Za-z0-9_-]{32,128}', client['token']):
        raise ValueError('Invalid model client identity')

    # The public resource version changes the Pod template on explicit refresh.
    # A new Pod gets a new emptyDir, while CLI refreshes survive container restarts.
    resource = json.loads(run(kubectl + ["get", "secret", "ai-sdlc-codex-auth", "-n", "ai-sdlc-runtime", "-o", "json"], capture_output=True).stdout)
    revision = resource.get("metadata", {}).get("resourceVersion", "")
    if not re.fullmatch(r"[0-9]{1,64}", revision):
        raise ValueError("Invalid model credential resource version")
    return revision


def api_cidrs(kubectl):
    from install import run
    service = json.loads(run(kubectl + ['get', 'service', 'kubernetes', '-n', 'default', '-o', 'json'], capture_output=True).stdout)
    endpoints = json.loads(run(kubectl + ['get', 'endpoints', 'kubernetes', '-n', 'default', '-o', 'json'], capture_output=True).stdout)
    addresses = [service['spec']['clusterIP']] + [a['ip'] for s in endpoints.get('subsets', []) for a in s.get('addresses', [])]
    if not 2 <= len(addresses) <= 8:
        raise ValueError('Unsupported Kubernetes API endpoint inventory')
    return sorted({str(ipaddress.IPv4Address(value)) + '/32' for value in addresses})


def review_credentials(kubectl, cluster_uid, installer=None):
    from install import owned_secret, create_owned_secret
    if installer is not None:
        owned_secret, create_owned_secret = installer.owned_secret, installer.create_owned_secret
    current = owned_secret(kubectl, 'ai-sdlc-review-model-client', cluster_uid)
    if current is None:
        create_owned_secret(kubectl, 'ai-sdlc-review-model-client', {'token': secrets.token_urlsafe(48)}, cluster_uid)
    elif set(current) != {'token'} or not re.fullmatch(r'[A-Za-z0-9_-]{32,128}', current['token']):
        raise ValueError('Invalid independent review model client identity')


def apply_model_upstream(values, path):
    """Select a reviewed OpenAI-compatible model service at installation: the
    gateways use the openai adapter against one private endpoint, and the
    executor and reviewer profiles take its per-call windows. The document is
    validated exactly like the model-upstream runtime upgrade's."""
    import model_upstream_runtime_upgrade as upstreams
    with Path(path).open('rb') as source:
        raw = source.read(4097)
    if len(raw) > 4096:
        raise ValueError('Model upstream document exceeds 4 KiB')
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError('Duplicate model upstream field')
            result[key] = value
        return result
    try:
        document = json.loads(raw, object_pairs_hook=unique)
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise ValueError('Invalid model upstream JSON') from exc
    upstream = upstreams.validate_upstream(document)
    if upstream['credentialSecret']:
        raise ValueError('An installation-time model service credential is not supported yet; use the client allowlist')
    config = json.loads(values['candidate']['configJSON'])
    if upstream['callSeconds'] > config['profile']['maxSeconds']:
        raise ValueError('Model call window exceeds the executor profile')
    config['profile']['callSeconds'] = upstream['callSeconds']
    # One model call per SCC step: the call budget matches the step budget
    # (each call reserves the response limit of output). The bundled Codex
    # installations keep their reviewed 16-call budget and transitions.
    steps = config['profile']['maxSteps']
    config['model']['Limits'] = {'Calls': steps, 'InputBytes': 4194304, 'OutputBytes': steps * config['model']['ResponseLimit']}
    values['candidate']['configJSON'] = json.dumps(config, sort_keys=True)
    if 'review' in values:
        review = json.loads(values['review']['configJSON'])
        if upstream['reviewCallSeconds'] > review['profile']['maxSeconds']:
            raise ValueError('Model call window exceeds the reviewer profile')
        review['profile']['callSeconds'] = upstream['reviewCallSeconds']
        values['review']['configJSON'] = json.dumps(review, sort_keys=True)
    # The benchmark's answer envelope follows the answer bound, so the derived
    # per-call timeout covers the longest answer the profile admits.
    values['modelGateway'] = upstreams.gateway_values(upstream, config['profile']['maxTokens'])
    return values
