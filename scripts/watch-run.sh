#!/bin/sh
# Watch a long-running command, such as a live download, and return when it
# ends, when it stalls, or at a checkpoint while it is still working.
#
# Run the watcher in the background (an agent's background task, or `&` in a
# shell): its exit is the signal to look at the command again, so nobody
# waits for hours on a run that has hung. The command runs detached in its own
# session and outlives the watcher; after a checkpoint, attach again.
#
# A stall is --stall seconds in which the log did not grow and the command and
# all of its children, Chrome included, used less than half a second of CPU
# and none of them exited.
# The application prints a line for every retry and publisher pause, so a
# publisher that stops answering for minutes is not a stall here. With
# --quit-on-stall the command gets SIGQUIT, which makes a Go program write the
# stack of every goroutine to the log before it exits, so a deadlock shows
# where it waits.
set -u

usage() {
	cat <<'EOF'
Usage:
  scripts/watch-run.sh [options] --log FILE -- COMMAND [ARG...]
  scripts/watch-run.sh [options] --log FILE --attach
  scripts/watch-run.sh [options] --log FILE --pid PID

The first form starts COMMAND detached, with its output in FILE, its process
ID in FILE.pid and its exit status in FILE.exit once it ends. --attach watches
the command started earlier for FILE; --pid watches any process writing FILE.

The watcher returns:
  the command's exit status   when the command ends
  75                          at a checkpoint: still running; attach again
  76                          on a stall
  70                          when the command ended with an unknown status
  64                          on a usage error
It prints a summary: guide results, the last lines of FILE and, on a stall,
the process tree and its network sockets. A heartbeat line is appended to
FILE.watch at every sample.

Options:
  --interval SECONDS    sampling interval (default 15)
  --stall SECONDS       quiet time that counts as a stall (default 300)
  --checkpoint SECONDS  return after this long while still running
                        (default 600; 0 never)
  --quit-on-stall       on a stall, send SIGQUIT to the command so that a Go
                        program writes all goroutine stacks to FILE
  --quit-match TEXT     with --quit-on-stall, send it instead to every process
                        in the tree whose executable name contains TEXT, such
                        as a program that a driver script runs
EOF
}

die() {
	echo "watch-run: $*" >&2
	exit 64
}

interval=15
stall=300
checkpoint=600
quit=0
match=
log=
attach=0
pid=
while [ $# -gt 0 ]; do
	case $1 in
	--log) [ $# -ge 2 ] || die "--log needs a file"; log=$2; shift 2 ;;
	--interval) [ $# -ge 2 ] || die "--interval needs seconds"; interval=$2; shift 2 ;;
	--stall) [ $# -ge 2 ] || die "--stall needs seconds"; stall=$2; shift 2 ;;
	--checkpoint) [ $# -ge 2 ] || die "--checkpoint needs seconds"; checkpoint=$2; shift 2 ;;
	--quit-on-stall) quit=1; shift ;;
	--quit-match) [ $# -ge 2 ] || die "--quit-match needs text"; match=$2; shift 2 ;;
	--attach) attach=1; shift ;;
	--pid) [ $# -ge 2 ] || die "--pid needs a process ID"; pid=$2; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	--) shift; break ;;
	*) usage >&2; exit 64 ;;
	esac
done
for value in "$interval" "$stall" "$checkpoint"; do
	case $value in '' | *[!0-9]*) die "durations are whole seconds: $value" ;; esac
done
[ "$interval" -gt 0 ] || die "--interval must be positive"
[ -n "$log" ] || die "--log is required"
modes=0
[ $# -gt 0 ] && modes=$((modes + 1))
[ "$attach" = 1 ] && modes=$((modes + 1))
[ -n "$pid" ] && modes=$((modes + 1))
[ "$modes" = 1 ] || die "give exactly one of: -- COMMAND, --attach, --pid"

wrapped=1
if [ $# -gt 0 ]; then
	mkdir -p "$(dirname "$log")" || exit 64
	: >"$log" || exit 64
	rm -f "$log.exit" "$log.pid" "$log.watch"
	# A new session keeps the command running when the watcher, or the task
	# that ran it, ends. GOTRACEBACK=all makes SIGQUIT dumps complete.
	GOTRACEBACK=all perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV or die "watch-run: cannot start: $!\n"' \
		/bin/sh -c 'log=$1; shift; "$@" >>"$log" 2>&1 </dev/null; echo $? >"$log.exit"' watch-run "$log" "$@" \
		</dev/null >/dev/null 2>&1 &
	pid=$!
	echo "$pid" >"$log.pid"
	echo "watch-run: started pid $pid; output in $log"
elif [ "$attach" = 1 ]; then
	pid=$(cat "$log.pid" 2>/dev/null) || die "no $log.pid: start the command with this script first"
else
	wrapped=0
fi
case $pid in '' | *[!0-9]*) die "invalid process ID: $pid" ;; esac

# tree lists the process and all of its descendants.
tree() {
	todo=$1
	all=
	while :; do
		set -- $todo
		[ $# -gt 0 ] || break
		head=$1
		shift
		todo="$* $(pgrep -P "$head" 2>/dev/null | tr '\n' ' ')"
		all="$all $head"
	done
	echo $all
}

# cpu prints the CPU seconds used so far by the process tree.
cpu() {
	pids=$(tree "$pid" | tr ' ' ',')
	[ -n "$pids" ] || { echo 0; return; }
	ps -o time= -p "$pids" 2>/dev/null | awk '{
		sub(/^[0-9]+-/, "", $1); n = split($1, part, ":"); s = 0
		for (i = 1; i <= n; i++) s = s * 60 + part[i]
		total += s
	} END { printf "%.2f\n", total + 0 }'
}

size() {
	wc -c <"$log" 2>/dev/null | tr -d ' ' || echo 0
}

duration() {
	printf '%dm%02ds' $(($1 / 60)) $(($1 % 60))
}

summary() {
	echo "--- guide results"
	grep -E '^\[[0-9]+/[0-9]+\] [^:]*: (complete|degraded|incomplete|failed) \(' "$log" 2>/dev/null | tail -n 40
	echo "--- last lines of $log"
	tail -n "$1" "$log" 2>/dev/null | cut -c1-300
}

began=$(date +%s)
changed=$began
changed_size=$(size)
changed_cpu=$(cpu)
while :; do
	waited=0
	while [ "$waited" -lt "$interval" ] && kill -0 "$pid" 2>/dev/null; do
		sleep 1
		waited=$((waited + 1))
	done
	now=$(date +%s)
	if ! kill -0 "$pid" 2>/dev/null; then
		status=unknown
		[ "$wrapped" = 1 ] && status=$(cat "$log.exit" 2>/dev/null || echo unknown)
		echo "WATCH: finished with exit status $status after $(duration $((now - began))) of watching"
		summary 12
		case $status in '' | *[!0-9]*) exit 70 ;; esac
		exit "$status"
	fi
	current_size=$(size)
	current_cpu=$(cpu)
	if [ "$current_size" != "$changed_size" ] ||
		awk -v now="$current_cpu" -v then="$changed_cpu" 'BEGIN { exit !(now - then >= 0.5 || now < then) }'; then
		changed=$now
		changed_size=$current_size
		changed_cpu=$current_cpu
	fi
	quiet=$((now - changed))
	printf '%s watched=%s cpu=%ss log=%sB quiet=%ss | %s\n' "$(date +%T)" "$(duration $((now - began)))" \
		"$current_cpu" "$current_size" "$quiet" "$(tail -n 1 "$log" 2>/dev/null | cut -c1-200)" >>"$log.watch"
	if [ "$quiet" -ge "$stall" ]; then
		pids=$(tree "$pid" | tr ' ' ',')
		echo "WATCH: stalled: no output and under 0.5 s of CPU for $(duration "$quiet")"
		echo "--- processes"
		ps -o pid,ppid,etime,time,%cpu,state,command -p "$pids" 2>/dev/null | cut -c1-200
		echo "--- open network sockets: $(lsof -a -p "$pids" -i -n -P 2>/dev/null | awk 'NR > 1' | wc -l | tr -d ' ')"
		summary 8
		if [ "$quit" = 1 ]; then
			targets=$pid
			[ "$wrapped" = 1 ] && targets=$(pgrep -P "$pid" | tr '\n' ' ')
			if [ -n "$match" ]; then
				targets=
				for candidate in $(tree "$pid"); do
					case $(ps -o comm= -p "$candidate" 2>/dev/null) in *"$match"*) targets="$targets $candidate" ;; esac
				done
			fi
			echo "--- sent SIGQUIT to $targets; goroutines that wait in application code:"
			kill -QUIT $targets 2>/dev/null
			waited=0
			while [ "$waited" -lt 15 ] && [ -n "$targets" ] && kill -0 $targets 2>/dev/null; do
				sleep 1
				waited=$((waited + 1))
			done
			sed -n '/^SIGQUIT/,$p' "$log" | grep -E '^goroutine [0-9]+ .*\[|^main\.|aos-cx-docs-dldr/' | head -n 80
		fi
		exit 76
	fi
	if [ "$checkpoint" -gt 0 ] && [ $((now - began)) -ge "$checkpoint" ]; then
		echo "WATCH: checkpoint: still running after $(duration $((now - began))) of watching; last change $(duration "$quiet") ago"
		summary 6
		exit 75
	fi
done
