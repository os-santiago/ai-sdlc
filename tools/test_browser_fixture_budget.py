import subprocess
import sys
import time
import unittest

from browser_fixture_budget import Budget


class CleanupBudget(unittest.TestCase):
    def test_real_command_can_clean_after_work_expiry_without_renewal(self):
        budget=Budget();budget.deadline=time.monotonic()-1
        with self.assertRaises(ValueError):budget.timeout(30)
        budget.start_cleanup()
        result=subprocess.run([sys.executable,'-c','import time;time.sleep(.02);print("contained")'],
            capture_output=True,check=True,timeout=budget.timeout(2))
        self.assertEqual(result.stdout,b'contained\n')
        budget.cleanup_deadline=time.monotonic()-1
        with self.assertRaises(ValueError):budget.timeout(30)
        with self.assertRaises(ValueError):budget.start_cleanup()


if __name__=='__main__':unittest.main()
