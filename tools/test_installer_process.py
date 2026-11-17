"""Real process output and descendant bounds for installer commands."""
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import installer_process as process

class ProcessTests(unittest.TestCase):
    def test_oversized_output_is_rejected_before_unbounded_capture(self):
        with self.assertRaises(ValueError):
            process.capture([sys.executable,'-c','import sys;sys.stdout.buffer.write(b"x"*(8*1024*1024))'])
        self.assertEqual(process.capture([sys.executable,'-c','print("complete")']),b'complete\n')

    def test_stalled_descendant_pipe_ends_with_original_budget(self):
        with tempfile.TemporaryDirectory() as directory:
            pidfile=Path(directory)/'child'
            script="import subprocess,sys;p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);open(sys.argv[1],'w').write(str(p.pid))"
            started=time.monotonic()
            with self.assertRaises(ValueError):process.capture([sys.executable,'-c',script,str(pidfile)],timeout=.2)
            self.assertLess(time.monotonic()-started,2)
            pid=int(pidfile.read_text())
            for _ in range(100):
                stat=Path('/proc')/str(pid)/'stat'
                if not stat.exists() or stat.read_text().split()[2]=='Z':break
                time.sleep(.01)
            else:self.fail('Owned descendant survived output deadline')

    def test_closed_stdout_does_not_let_live_process_escape_its_budget(self):
        started=time.monotonic()
        with self.assertRaises(ValueError):
            process.capture([sys.executable,'-c','import os,time;os.close(1);time.sleep(60)'],timeout=.1)
        self.assertLess(time.monotonic()-started,2)

    def test_lost_monitor_prevents_launch(self):
        def stopped():raise ValueError('original fence unavailable')
        with patch.object(process.subprocess,'Popen') as start:
            with self.assertRaises(ValueError):process.capture(['must-not-run'],monitor=stopped)
            start.assert_not_called()

    def test_original_session_leader_remains_reserved_until_cleanup(self):
        original=os.killpg;calls=[]
        def kill(pid,signal):
            self.assertIsNotNone(os.waitid(os.P_PID,pid,os.WEXITED|os.WNOHANG|os.WNOWAIT))
            calls.append(pid);original(pid,signal)
        with patch.object(process.os,'killpg',side_effect=kill):process.capture([sys.executable,'-c','pass'])
        self.assertEqual(len(calls),1)

if __name__=='__main__':unittest.main()
