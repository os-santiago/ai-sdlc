"""Misplaced delegated commands must never enter ordinary local installation."""
import unittest
from unittest.mock import patch
import install

class DispatchTests(unittest.TestCase):
    def test_misplaced_subcommands_reject_before_config_or_effects(self):
        for command in ('supply-chain','build-foundation','registry-node','release-target'):
            with self.subTest(command=command),patch.object(install.install_config,'resolve') as resolve,patch.object(install,'tools') as tools,patch.object(install,'run') as run:
                with self.assertRaisesRegex(ValueError,'first argument'):
                    install.main(['--cluster','fixture',command])
                resolve.assert_not_called();tools.assert_not_called();run.assert_not_called()

if __name__=='__main__':unittest.main()
