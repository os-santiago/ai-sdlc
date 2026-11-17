"""Separate finite execution and original-resource cleanup budgets for fixtures."""
import time


class Budget:
    def __init__(self):
        self.deadline = time.monotonic() + 420
        self.cleanup_deadline = None

    def start_cleanup(self):
        if self.cleanup_deadline is not None:
            raise ValueError('Fixture cleanup budget cannot be renewed')
        self.cleanup_deadline = time.monotonic() + 180

    def timeout(self, limit):
        deadline = self.cleanup_deadline if self.cleanup_deadline is not None else self.deadline
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError('Original fixture phase budget expired')
        return min(limit, remaining)
