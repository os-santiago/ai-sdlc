"""Schema36 metadata from an independently owned PostgreSQL migration fixture."""
import json
from pathlib import Path

from browser_schema import canonical
from diagnostic_schema import QUERY as ORIGINAL_QUERY

QUERY = ORIGINAL_QUERY.replace('public.execution_failures', 'public.review_feedback')
EXPECTED = json.loads((Path(__file__).resolve().parent / 'fixtures/review-feedback/schema36.json').read_text())['review_feedback']


def verify(database):
    if canonical(json.loads(database.sql(QUERY))) != canonical(EXPECTED):
        raise ValueError('Feedback table structure, row security or privileges changed')
