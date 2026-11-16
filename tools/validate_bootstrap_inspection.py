#!/usr/bin/env python3
"""Validate bootstrap inspection after real maintenance-role provisioning.

Use the existing owned, disposable PostgreSQL runner. Database role names are
global, so this case deliberately receives its own PostgreSQL instance rather
than sharing the full role suite's cluster. It never contacts an installation.
"""
import validate_runtime as runtime


class ProvisionedFixture(runtime.Run):
    def stage(self,name,args,limit=900):
        if name=='runner-start':
            args=list(args)
            position=args.index(self.lock['go'])
            args[position:position]=['-e','AI_SDLC_BOOTSTRAP_PROVISIONED_FIXTURE=true']
        if name=='test':
            args=['exec',self.names[1],'go','test','-json','-count=1','./internal/store',
                '-run','^TestBootstrapInspectionProvisionedMaintenance$']
        return super().stage(name,args,limit)


if __name__=='__main__':
    runtime.Run=ProvisionedFixture
    runtime.REQUIRED_TESTS={('internal/store','TestBootstrapInspectionProvisionedMaintenance')}
    raise SystemExit(runtime.main())
