import hashlib
import json
import unittest
from release_target_oci import verify
class BindingTests(unittest.TestCase):
    def fixture(self,platform='amd64'):
        blobs={}
        def put(value):
            raw=value if isinstance(value,bytes) else json.dumps(value).encode();digest='sha256:'+hashlib.sha256(raw).hexdigest();blobs[digest]=raw;return {'digest':digest,'size':len(raw)}
        config=put({'os':'linux','architecture':platform});layer=put(b'actual synthetic layer bytes')
        manifest=put({'schemaVersion':2,'config':config,'layers':[layer]})
        index=put({'schemaVersion':2,'manifests':[manifest]})
        return blobs,manifest['digest'],index['digest'],put
    def test_exact_manifest_and_single_wrapper_bind_both_directions(self):
        b,m,i,_=self.fixture()
        for reference,runtime in ((m,i),(i,m),(m,m),(i,i)):
            result=verify(reference,runtime,lambda digest,limit:b[digest]);self.assertTrue(result['allContentHashesVerified']);self.assertEqual(result['manifestDigest'],m)
    def test_foreign_platform_multiple_manifests_and_corrupt_content_fail(self):
        b,m,i,put=self.fixture('arm64')
        with self.assertRaises(ValueError):verify(m,i,lambda digest,limit:b[digest])
        b,m,i,put=self.fixture();wrong=put({'schemaVersion':2,'manifests':[{'digest':m,'size':len(b[m])}]*2})['digest']
        with self.assertRaises(ValueError):verify(m,wrong,lambda digest,limit:b[digest])
        b,m,i,put=self.fixture();b[m]=b'corrupted'
        with self.assertRaises(ValueError):verify(m,i,lambda digest,limit:b[digest])
