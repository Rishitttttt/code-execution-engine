package executor

// sandboxScript is the /bin/sh program that runs as PID 1 inside every
// sandbox container. It is deliberately POSIX sh so the same text works on
// busybox (alpine images) and GNU coreutils (the gcc image).
//
// Contract with the worker:
//
//   - Source code arrives base64-encoded in OCEE_CODE_B64 rather than being
//     copied in. A tmpfs workdir is mounted at container start, which happens
//     *after* CopyToContainer would have written, so a copied file would be
//     shadowed by the empty tmpfs. Passing it through the environment sidesteps
//     that ordering trap entirely and leaves stdin free for the program.
//
//   - The script always exits 0. The user program's real exit status travels
//     back on the marker line instead, so a missing marker unambiguously means
//     the container itself was killed (OOM, daemon stop, wall-clock kill).
//
//   - The last line written to stderr is the marker:
//     <nonce> phase=<compile|run> rc=<n> cpu_usec=<n> mem_bytes=<n> secs=<n> compile_b64=<...>
//     The nonce is unguessable per-run, so a program printing something that
//     looks like a marker cannot spoof its own metrics.
const sandboxScript = `set -u

cd "$OCEE_WORKDIR" 2>/dev/null || { echo "ocee: workdir unavailable" >&2; exit 0; }

cpu_usec() {
  if [ -r /sys/fs/cgroup/cpu.stat ]; then
    awk '/^usage_usec/ {print $2; f=1} END {if (!f) print 0}' /sys/fs/cgroup/cpu.stat
  elif [ -r /sys/fs/cgroup/cpuacct/cpuacct.usage ]; then
    awk '{printf "%d\n", $1 / 1000}' /sys/fs/cgroup/cpuacct/cpuacct.usage
  else
    echo 0
  fi
}

# cgroup v2 exposes a true high-water mark (memory.peak, kernel 5.19+); v1 has
# max_usage_in_bytes. memory.current is the last-resort fallback and reads low
# because it is an instantaneous sample, so it is reported as-is rather than
# guessed at.
mem_peak() {
  for f in /sys/fs/cgroup/memory.peak \
           /sys/fs/cgroup/memory/memory.max_usage_in_bytes \
           /sys/fs/cgroup/memory.current; do
    if [ -r "$f" ]; then cat "$f"; return; fi
  done
  echo 0
}

emit() {
  printf '\n%s phase=%s rc=%s cpu_usec=%s mem_bytes=%s secs=%s compile_b64=%s\n' \
    "$OCEE_NONCE" "$1" "$2" "$3" "$(mem_peak)" "$4" "$5" >&2
}

printf '%s' "$OCEE_CODE_B64" | base64 -d > "$OCEE_FILE" 2>/dev/null || {
  echo "ocee: could not decode submission" >&2
  emit setup 70 0 0 ""
  exit 0
}

if [ -n "$OCEE_COMPILE" ]; then
  timeout -s KILL "$OCEE_COMPILE_SECS" /bin/sh -c "$OCEE_COMPILE" >.ocee_compile 2>&1 </dev/null
  crc=$?
  if [ "$crc" -ne 0 ]; then
    # Compiler diagnostics ride back base64-encoded on the marker so they stay
    # separate from the program's own stderr, which is empty in this case.
    emit compile "$crc" "$(cpu_usec)" 0 "$(head -c 16384 .ocee_compile | base64 | tr -d '\n')"
    exit 0
  fi
fi

# Snapshot after compiling so the reported CPU time is the program's alone.
base_cpu=$(cpu_usec)
t0=$(date +%s)

timeout -s KILL "$OCEE_RUN_SECS" /bin/sh -c "$OCEE_RUN"
rc=$?

t1=$(date +%s)
end_cpu=$(cpu_usec)

emit run "$rc" "$((end_cpu - base_cpu))" "$((t1 - t0))" ""
exit 0
`
