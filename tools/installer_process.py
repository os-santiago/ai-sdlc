"""Bound one installer subprocess group without limiting the provider daemon."""
import math
import os
import selectors
import signal
import subprocess
import time

MAX_OUTPUT=4*1024*1024


def capture(argv,*,timeout=30,monitor=lambda:None,input=None,env=None):
    if type(timeout) not in (int,float) or not math.isfinite(timeout) or not 0<timeout<=120:
        raise ValueError('Finite bounded installer command required')
    if input is not None and (not isinstance(input,bytes) or len(input)>16384):
        raise ValueError('Bounded private installer input required')
    pending=input
    monitor();deadline=time.monotonic()+timeout;raw=bytearray()
    child=subprocess.Popen(list(map(str,argv)),stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,stdout=subprocess.PIPE,
                           stderr=subprocess.DEVNULL,start_new_session=True,bufsize=0,env=env)
    def remaining():
        monitor();left=deadline-time.monotonic()
        if left<=0:raise ValueError('Installer command exceeded its original budget')
        return min(.05,left)
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(child.stdout,selectors.EVENT_READ)
            if child.stdin is not None:
                os.set_blocking(child.stdin.fileno(),False)
                if pending:selector.register(child.stdin,selectors.EVENT_WRITE)
                else:child.stdin.close()
            while selector.get_map():
                events=selector.select(remaining())
                for key,_ in events:
                    remaining()
                    if key.fileobj is child.stdin:
                        try:written=os.write(child.stdin.fileno(),pending[:8192])
                        except BlockingIOError:continue
                        if written<=0:raise ValueError('Private installer input stopped')
                        pending=pending[written:]
                        if not pending:selector.unregister(child.stdin);child.stdin.close()
                    else:
                        chunk=os.read(child.stdout.fileno(),min(65536,MAX_OUTPUT+1-len(raw)))
                        if not chunk:selector.unregister(child.stdout);continue
                        raw.extend(chunk)
                        if len(raw)>MAX_OUTPUT:raise ValueError('Installer output exceeded bound')
        # Do not reap the session leader until its entire owned group is stopped.
        while True:
            delay=remaining()
            status=os.waitid(os.P_PID,child.pid,os.WEXITED|os.WNOHANG|os.WNOWAIT)
            if status is not None:break
            time.sleep(delay)
        if status.si_code!=os.CLD_EXITED or status.si_status:
            raise ValueError('Original installer command failed')
        return bytes(raw)
    finally:
        if child.stdin is not None:child.stdin.close()
        child.stdout.close()
        try:os.killpg(child.pid,signal.SIGKILL)
        except ProcessLookupError:pass
        child.wait(timeout=5)
