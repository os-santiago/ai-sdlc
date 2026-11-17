import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
import release_target_oci as oci

class BoundedOCIReadTests(unittest.TestCase):
    def test_exact_limit_and_provider_state_file_remain_independent(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'provider-state'
            code="from pathlib import Path;import sys;Path(sys.argv[1]).write_bytes(b's'*1048576);sys.stdout.buffer.write(b'1234')"
            self.assertEqual(oci.bounded_output([sys.executable,'-c',code,str(path)],4),b'1234')
            self.assertEqual(path.stat().st_size,1048576)

    def test_oversized_output_rejected_before_unbounded_producer_finishes(self):
        start=time.monotonic()
        with self.assertRaisesRegex(ValueError,'exceeded'):
            oci.bounded_output([sys.executable,'-c',"import os,time;os.write(1,b'x'*65536);time.sleep(30)"],8)
        self.assertLess(time.monotonic()-start,3)

    def test_stalled_reader_and_nonzero_exit_are_not_content(self):
        start=time.monotonic()
        with self.assertRaisesRegex(ValueError,'timed out'):
            oci.bounded_output([sys.executable,'-c','import time;time.sleep(30)'],8,timeout=.15)
        self.assertLess(time.monotonic()-start,3)
        with self.assertRaisesRegex(ValueError,'unavailable'):
            oci.bounded_output([sys.executable,'-c',"import sys;sys.stdout.write('ok');sys.exit(1)"],8)

    def test_group_cleanup_precedes_reaping_and_pid_remains_reserved(self):
        events=[];real_popen=oci.subprocess.Popen;real_kill=oci.os.killpg
        def launch(*args,**kwargs):
            process=real_popen(*args,**kwargs);wait=process.wait
            def reap(*args,**kwargs):events.append('reap');return wait(*args,**kwargs)
            process.wait=reap;return process
        def kill(pid,signal):
            # Raises ChildProcessError if the implementation reaped too early.
            status=os.waitid(os.P_PID,pid,os.WEXITED|os.WNOHANG|os.WNOWAIT)
            self.assertEqual(status.si_pid,pid)
            events.append('kill');return real_kill(pid,signal)
        with patch.object(oci.subprocess,'Popen',side_effect=launch),patch.object(oci.os,'killpg',side_effect=kill):
            self.assertEqual(oci.bounded_output([sys.executable,'-c',"print('ok',end='')"],2),b'ok')
        self.assertEqual(events,['kill','reap'])

    def test_closed_stdout_does_not_allow_process_to_outlive_deadline(self):
        start=time.monotonic()
        with self.assertRaisesRegex(ValueError,'timed out'):
            oci.bounded_output([sys.executable,'-c','import os,time;os.close(1);time.sleep(30)'],8,timeout=.15)
        self.assertLess(time.monotonic()-start,3)

    def test_child_holding_pipe_is_killed_on_deadline(self):
        with tempfile.TemporaryDirectory() as directory:
            pid=Path(directory)/'pid'
            code="import subprocess,sys;from pathlib import Path;p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)']);Path(sys.argv[1]).write_text(str(p.pid))"
            with self.assertRaisesRegex(ValueError,'timed out'):
                oci.bounded_output([sys.executable,'-c',code,str(pid)],8,timeout=.2)
            child=int(pid.read_text());deadline=time.monotonic()+2
            while time.monotonic()<deadline:
                status=Path(f'/proc/{child}/stat')
                if not status.exists() or status.read_text().split()[2]=='Z':break
                time.sleep(.02)
            else:self.fail('Owned stalled child survived deadline')

if __name__=='__main__':unittest.main()
