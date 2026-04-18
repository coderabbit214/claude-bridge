#!/usr/bin/env python3

import fcntl
import os
import pty
import selectors
import signal
import sys
import termios
import time
import tty


def open_input_pipe(path: str) -> int | None:
    try:
        return os.open(path, os.O_RDONLY | os.O_NONBLOCK)
    except OSError:
        return None


def get_winsize(fd: int) -> bytes:
    return fcntl.ioctl(fd, termios.TIOCGWINSZ, b"\0" * 8)


def set_winsize(fd: int, winsize: bytes) -> None:
    fcntl.ioctl(fd, termios.TIOCSWINSZ, winsize)


def write_all(fd: int, data: bytes) -> None:
    view = memoryview(data)
    offset = 0
    while offset < len(view):
        offset += os.write(fd, view[offset:])


def submit_line(master_fd: int, line: bytes) -> None:
    chunk_size = 96
    for start in range(0, len(line), chunk_size):
        chunk = line[start:start + chunk_size]
        write_all(master_fd, chunk)
        if start + chunk_size < len(line):
            time.sleep(0.01)
    time.sleep(0.03)
    write_all(master_fd, b"\r")


def main() -> int:
    if len(sys.argv) < 4 or "--" not in sys.argv[1:]:
        print("usage: claude_bridge_mux.py <input-pipe> -- <command> [args...]", file=sys.stderr)
        return 2

    sep = sys.argv.index("--")
    input_pipe = sys.argv[1]
    cmd = sys.argv[sep + 1 :]
    if not cmd:
        print("missing command", file=sys.stderr)
        return 2

    pid, master_fd = pty.fork()
    if pid == 0:
        os.execvp(cmd[0], cmd)

    selector = selectors.DefaultSelector()
    stdin_fd = sys.stdin.fileno()
    stdout_fd = sys.stdout.fileno()
    selector.register(master_fd, selectors.EVENT_READ, "pty")
    stdin_registered = False
    try:
        selector.register(stdin_fd, selectors.EVENT_READ, "stdin")
        stdin_registered = True
    except OSError:
        stdin_registered = False

    pipe_fd = None
    pipe_buf = b""
    stdin_tty = os.isatty(stdin_fd)
    old_attrs = None
    if stdin_tty and stdin_registered:
        old_attrs = termios.tcgetattr(stdin_fd)
        tty.setraw(stdin_fd)

    def sync_window_size(*_args: object) -> None:
        try:
            set_winsize(master_fd, get_winsize(stdin_fd))
        except OSError:
            pass

    signal.signal(signal.SIGWINCH, sync_window_size)
    sync_window_size()

    try:
        while True:
            if pipe_fd is None:
                pipe_fd = open_input_pipe(input_pipe)
                if pipe_fd is not None:
                    selector.register(pipe_fd, selectors.EVENT_READ, "pipe")

            try:
                child_pid, status = os.waitpid(pid, os.WNOHANG)
            except ChildProcessError:
                break
            if child_pid == pid:
                if os.WIFEXITED(status):
                    return os.WEXITSTATUS(status)
                if os.WIFSIGNALED(status):
                    return 128 + os.WTERMSIG(status)
                return 0

            for key, _ in selector.select(timeout=0.2):
                source = key.data
                fd = key.fileobj if isinstance(key.fileobj, int) else key.fileobj.fileno()
                try:
                    data = os.read(fd, 4096)
                except OSError:
                    data = b""

                if not data:
                    if source == "pipe" and pipe_fd is not None:
                        if pipe_buf:
                            submit_line(master_fd, pipe_buf)
                            pipe_buf = b""
                        selector.unregister(pipe_fd)
                        os.close(pipe_fd)
                        pipe_fd = None
                        continue

                if source == "pty":
                    write_all(stdout_fd, data)
                else:
                    if source == "pipe":
                        pipe_buf += data
                        while b"\n" in pipe_buf:
                            line, pipe_buf = pipe_buf.split(b"\n", 1)
                            submit_line(master_fd, line)
                    else:
                        write_all(master_fd, data)
    finally:
        if old_attrs is not None:
            termios.tcsetattr(stdin_fd, termios.TCSADRAIN, old_attrs)
        if pipe_fd is not None:
            try:
                selector.unregister(pipe_fd)
            except Exception:
                pass
            os.close(pipe_fd)
        if stdin_registered:
            try:
                selector.unregister(stdin_fd)
            except Exception:
                pass
        try:
            selector.unregister(master_fd)
        except Exception:
            pass
        os.close(master_fd)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
