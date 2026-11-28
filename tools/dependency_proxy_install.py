"""Installer support for the optional in-cluster dependency proxy.

``--dependency-proxy maven``, ``npm`` or ``maven,npm`` renders the chart's
``dependencyProxy`` section (docs/operations/dependency-cache.md): the
ai-sdlc-dependencies namespace, a deny-all policy, and one pinned caching
proxy that is the namespace's only egress point. Omitting the option on a
later install removes the proxy and its cache claim.
"""
ECOSYSTEMS = ('maven', 'npm')


def parse(text):
    """The selected ecosystems, in canonical order."""
    selected = text.split(',') if isinstance(text, str) else []
    if not selected or len(set(selected)) != len(selected) or any(e not in ECOSYSTEMS for e in selected):
        raise ValueError('--dependency-proxy takes maven, npm or maven,npm')
    return tuple(e for e in ECOSYSTEMS if e in selected)


def values(ecosystems):
    """Chart values for the selection; the chart's defaults pin the image and
    the upstream hosts."""
    return {'enabled': True, 'maven': {'enabled': 'maven' in ecosystems}, 'npm': {'enabled': 'npm' in ecosystems}}
