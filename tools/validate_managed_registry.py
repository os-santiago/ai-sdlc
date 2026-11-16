#!/usr/bin/env python3
"""Exercise the installation probe twice against disposable native Zot.

Fixture-only credentials/trust and an additional fixture administrator seed a
second repository for cross-repository denial checks. This does not install or
authorize operational trust, and is not a Kubernetes persistence proof.
"""
import json

import managed_registry_probe
import validate_provenance_registry as native


class Run(native.Run):
    def prepare_configuration(self):
        self.publisher='konflux'
        credentials=json.loads((self.private/'credentials.json').read_text())
        credentials[self.publisher]=credentials.pop('ai-sdlc-publisher')
        (self.private/'credentials.json').write_text(json.dumps(credentials))
        hashes=(self.private/'htpasswd').read_text().replace('ai-sdlc-publisher:','konflux:')
        (self.private/'htpasswd').write_text(hashes)
        config=json.loads((self.private/'config.json').read_text())
        config['http']['accessControl']['repositories'][native.REPO]['policies'][0]['users']=[self.publisher]
        (self.private/'config.json').write_text(json.dumps(config))

    def after_seed(self, request, image_path, endpoint):
        def actual(method,path,identity,body=b'',media='application/octet-stream'):
            return request(method,path,{'publisher':self.publisher,'reader':'ai-sdlc-verifier',None:None}[identity],body,media)
        nonce=self.identity+'a'*16
        first=managed_registry_probe.check_matrix(actual,nonce)
        second=managed_registry_probe.check_matrix(actual,nonce,require_existing=True)
        if first['markerDigest']!=second['markerDigest']:
            raise native.FixtureError('Reapplication replaced administrative marker')
        self.report['administrativeProbe']={'first':first,'reapply':second}


if __name__=='__main__':
    native.Run=Run
    raise SystemExit(native.main())
