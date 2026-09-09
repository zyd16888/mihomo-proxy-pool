"""Stop the container if a supervised service exhausts its startup retries."""
import os
import sys
from supervisor import childutils

headers, payload = childutils.listener.wait(sys.stdin, sys.stdout)
childutils.listener.ok(sys.stdout)
childutils.getRPCInterface(os.environ).supervisor.shutdown()
