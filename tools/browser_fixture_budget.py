"""Separate finite execution and original-resource cleanup budgets for fixtures."""
import time

WORK_SECONDS = 420
CLEANUP_SECONDS = 180
CALLER_TIMEOUT = WORK_SECONDS + CLEANUP_SECONDS + 30


class Budget:
    def __init__(self):
        self.deadline = time.monotonic() + WORK_SECONDS
        self.cleanup_deadline = None

    def start_cleanup(self):
        if self.cleanup_deadline is not None:
            raise ValueError('Fixture cleanup budget cannot be renewed')
        self.cleanup_deadline = time.monotonic() + CLEANUP_SECONDS

    def timeout(self, limit):
        deadline = self.cleanup_deadline if self.cleanup_deadline is not None else self.deadline
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Original fixture phase budget expired')
        return min(limit, remaining)
