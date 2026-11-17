import subprocess
import ast
from pathlib import Path
import sys
import time
import unittest

from browser_fixture_budget import Budget
import browser_fixture_budget
import yaml


class CleanupBudget(unittest.TestCase):
    def test_ci_caller_preserves_execution_and_cleanup_windows(self):
        workflow=yaml.safe_load((Path(__file__).resolve().parents[1]/'.github/workflows/browser.yml').read_text())
        matches=[]
        for step in workflow['jobs']['isolated-browser']['steps']:
            script=step.get('run','')
            if 'tools/verify_browser_transport.py' not in script:continue
            program=script.split("python3 - <<'PY'\n",1)[1].rsplit('\nPY',1)[0]
            for node in ast.walk(ast.parse(program)):
                if (isinstance(node,ast.Call) and isinstance(node.func,ast.Attribute)
                        and isinstance(node.func.value,ast.Name) and node.func.value.id=='subprocess'
                        and node.func.attr=='run' and node.args and isinstance(node.args[0],ast.List)
                        and any(isinstance(x,ast.Constant) and x.value=='tools/verify_browser_transport.py' for x in node.args[0].elts)):
                    matches.append(next(x.value for x in node.keywords if x.arg=='timeout'))
        self.assertEqual(len(matches),1)
        self.assertEqual(ast.dump(matches[0]),ast.dump(ast.parse('browser_fixture_budget.CALLER_TIMEOUT',mode='eval').body))
        self.assertGreater(browser_fixture_budget.CALLER_TIMEOUT,browser_fixture_budget.WORK_SECONDS+browser_fixture_budget.CLEANUP_SECONDS)

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
