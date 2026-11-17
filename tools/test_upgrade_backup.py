"""Private archive custody and closed recovery intents; real PG proof is separate."""
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock,patch

import runtime_upgrade_backup as runtime
import upgrade_backup_data as data


class BackupTests(unittest.TestCase):
    def archives(self,path):
        return data.write_archives(path,{'database.dump':b'PGDMP fixture','globals.sql':b'-- private fixture'})

    def test_archive_permissions_hash_and_filesystem_identity(self):
        for fault in ('changed','public-file','symlink','hardlink','public-directory'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as directory:
                path=Path(directory);manifest=self.archives(path);data.verify_archives(path,manifest)
                item=path/'globals.sql'
                if fault=='changed':item.write_bytes(b'changed')
                elif fault=='public-file':item.chmod(0o644)
                elif fault=='symlink':
                    item.rename(path/'saved');item.symlink_to(path/'saved')
                elif fault=='hardlink':os.link(item,path/'linked')
                else:path.chmod(0o755)
                with self.assertRaises((ValueError,OSError)):data.verify_archives(path,manifest)

    def test_existing_archive_cannot_be_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory);manifest=self.archives(path)
            with self.assertRaises(FileExistsError):self.archives(path)
            data.verify_archives(path,manifest)

    def test_incomplete_intent_never_reexports_or_recreates_restore(self):
        item=object.__new__(runtime.Backup);item.record={'phase':'captured'};item.source=Mock();item.guard=Mock()
        with patch.object(runtime,'Restore') as restore:
            with self.assertRaises(ValueError):item.run()
            restore.assert_not_called();item.source.assert_not_called();item.guard.assert_not_called()

    def test_verified_status_checks_original_archives_without_writing(self):
        with tempfile.TemporaryDirectory() as directory:
            item=object.__new__(runtime.Backup);item.directory=Path(directory);item.guard=Mock();item.save=Mock()
            item.record={'phase':'verified','restore':{'state':'removed'},'archives':self.archives(item.directory),
                         'verifiedAt':1,'retainedData':{key:{'bytes' if key=='@schema' else 'rows':1,'sha256':'a'*64} for key in ('@schema','@roles','@membership','@role-settings')}}
            result=item.status();self.assertFalse(result['upgradeAuthorized']);self.assertFalse(result['rollbackAuthorized'])
            self.assertFalse(result['autonomousEndToEndValidated']);item.save.assert_not_called()
            (item.directory/'database.dump').write_bytes(b'changed')
            with self.assertRaises(ValueError):item.status()

    def test_restore_unknown_or_foreign_container_never_removed(self):
        image='docker.io/library/postgres@sha256:'+'a'*64
        for fault in ('missing','id','owner','network'):
            with self.subTest(fault=fault):
                record={'restore':{}};restore=runtime.Restore('podman',image,Path('/private'),record,Mock())
                if fault!='missing':restore.cid='a'*64
                obj={'Id':'b'*64 if fault=='id' else restore.cid,'Config':{'Labels':{runtime.LABEL:'foreign' if fault=='owner' else restore.owner}},
                     'HostConfig':{'NetworkMode':'bridge' if fault=='network' else 'none'}}
                with patch.object(data,'capture',return_value=json.dumps([obj]).encode()) as execute:
                    with self.assertRaises(ValueError):restore.close()
                    self.assertFalse(any('rm' in call.args[0] for call in execute.call_args_list))

    def test_snapshot_rejects_foreign_scope_and_identifier_injection_before_query(self):
        for fault in ('scope','identifier'):
            with self.subTest(fault=fault):
                identity={'database':'ai_sdlc' if fault=='identifier' else 'foreign','superuser':True,'customTablespaces':0}
                execute=Mock(side_effect=[json.dumps(identity).encode(),json.dumps(['name;DROP TABLE work_items']).encode()])
                db=data.Database(execute,['owned','exec'])
                with self.assertRaises(ValueError):db.snapshot()
                self.assertEqual(execute.call_count,1 if fault=='scope' else 2)

if __name__=='__main__':unittest.main()
