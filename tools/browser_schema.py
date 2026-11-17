"""Exact schema35 metadata, observed only in a disposable reviewed migration."""
import copy
import json
from pathlib import Path
from diagnostic_schema import QUERY

EXPECTED=json.loads((Path(__file__).resolve().parent/'fixtures/browser/schema35.json').read_text())


def canonical(value):
    value=copy.deepcopy(value)
    # Index OIDs are physical restore details. Retain every index and flag,
    # comparing their semantic multiset rather than an OID-dependent order.
    if value.get('indexes') is not None:value['indexes'].sort()
    return value


def verify(database):
    for name,expected in EXPECTED.items():
        actual=json.loads(database.sql(QUERY.replace('public.execution_failures','public.'+name)))
        if canonical(actual)!=canonical(expected):
            raise ValueError('Browser table structure, row security or privileges changed')
