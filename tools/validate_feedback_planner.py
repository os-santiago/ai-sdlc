#!/usr/bin/env python3
"""Prove planner-produced blocked history on its own disposable PostgreSQL."""
import validate_runtime


class PlannerValidation(validate_runtime.Run):
    def stage(self, name, args, limit=900):
        if name in ('build', 'vet'):
            args = [*args[:-1], './internal/store']
        if name == 'test':
            # Cluster-wide component role names must never be adopted from the
            # shared full-suite fixture. Run only this case in a fresh instance.
            args = [args[0], '--env', 'AI_SDLC_FEEDBACK_PLANNER_FIXTURE=true', *args[1:-1],
                    '-run', '^TestUpgradeFenceFeedbackPlannerBlockedHistory$', './internal/store']
        return super().stage(name, args, limit)


def main(argv=None):
    validate_runtime.REQUIRED_TESTS = {('internal/store', 'TestUpgradeFenceFeedbackPlannerBlockedHistory')}
    validate_runtime.Run = PlannerValidation
    return validate_runtime.main(argv)


if __name__ == '__main__':
    raise SystemExit(main())
