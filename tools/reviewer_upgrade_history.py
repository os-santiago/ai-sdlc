"""One shared SQL contract for the installer and its administrative Go fence."""
from pathlib import Path

QUERY = (Path(__file__).resolve().parents[1] /
         'runtime/k8s/internal/store/reviewer_upgrade_history.sql').read_text()


def quiet(database):
    if database.sql(QUERY) != 't\n':
        raise ValueError('Reviewer update requires expired, cleaned failed review history and no downstream work')
