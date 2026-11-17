"""Read the original chart from immutable Git objects for legacy custody checks."""
from contextlib import contextmanager
import hashlib
from pathlib import Path, PurePosixPath
import tempfile

import install
import install_images
import install_upgrade

PREFIX = 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime/'


def digest(directory):
    result=hashlib.sha256()
    for path in sorted(Path(directory).rglob('*')):
        if path.is_symlink():raise ValueError('Chart symbolic link rejected')
        if path.is_file():result.update(str(path.relative_to(directory)).encode()+b'\0'+path.read_bytes()+b'\0')
    return result.hexdigest()


class OriginalChart:
    def __init__(self, root, revision, expected):
        if not install_images.SHA.fullmatch(revision) or len(expected)!=64:
            raise ValueError('Original immutable chart identity required')
        self.temporary=tempfile.TemporaryDirectory(prefix='ai-sdlc-original-chart-')
        self.path=Path(self.temporary.name)
        source=install_upgrade.Source(root)
        try:
            entries=source.git('ls-tree','-rz',revision,'--',PREFIX).split(b'\0')
            if entries[-1]!=b'' or not 1<=len(entries)-1<=128:raise ValueError('Original bounded chart inventory required')
            total=0
            for entry in entries[:-1]:
                header,name=entry.decode('ascii').split('\t',1);mode,kind,blob=header.split(' ')
                relative=PurePosixPath(name.removeprefix(PREFIX))
                if not name.startswith(PREFIX) or relative.is_absolute() or '..' in relative.parts or mode!='100644' or kind!='blob':
                    raise ValueError('Noncanonical chart entry')
                raw=source.git('cat-file','blob',blob);total+=len(raw)
                if total>4*1024*1024:raise ValueError('Original chart exceeds bound')
                destination=self.path/relative;destination.parent.mkdir(parents=True,exist_ok=True);destination.write_bytes(raw)
            if digest(self.path)!=expected:raise ValueError('Source chart differs from original activation custody')
            self.expected=expected
        except BaseException:
            self.close();raise

    @contextmanager
    def selected(self):
        # A local installer holds its installation lock and executes sequentially.
        # This scope cannot change Git, parent journals, or the target chart.
        if digest(self.path)!=self.expected:raise ValueError('Original chart extraction changed')
        current=install.CHART
        try:
            install.CHART=self.path
            yield
        finally:install.CHART=current

    def close(self):self.temporary.cleanup()
