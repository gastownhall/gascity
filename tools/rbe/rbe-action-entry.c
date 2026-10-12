/*
 * rbe-action-entry.c: the two tiny static binaries around the root launcher
 * (README "Action isolation"). Built at provision time; static, so no dynamic
 * loader ever reads LD_PRELOAD, LD_LIBRARY_PATH, GCONV_PATH & co. from the
 * action's environment:
 *   gcc -static -O2 -Wall -Wextra -o entry rbe-action-entry.c
 *   gcc -static -O2 -Wall -Wextra -DRBE_ACTION_EXEC -o exec rbe-action-entry.c
 *
 * entry (NativeLink `entrypoint`): runs as the worker user in the action's
 *   working directory with the action's environment, both chosen by the
 *   action. Never interprets its arguments and never runs anything but sudo,
 *   with a fixed clean environment; the root launcher gets the action's
 *   environment and argv as plain arguments:
 *     sudo -n -- LAUNCH <n> <env_1> ... <env_n> <argv_1> ...
 *
 * exec (RBE_ACTION_EXEC): the launcher's last step, already running as the
 *   slot user with no privileges:  exec <n> <env...> <argv...>
 *   runs argv with exactly that environment; PATH lookup uses the action's
 *   PATH, as NativeLink's own spawn did. Grants nothing to its caller.
 */
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#ifndef LAUNCH
#define LAUNCH "/usr/local/libexec/rbe-action/launch"
#endif
#ifndef SUDO
#define SUDO "/usr/bin/sudo"
#endif

extern char **environ;

#ifdef RBE_ACTION_EXEC
int main(int argc, char **argv) {
	char *end;
	if (argc < 3) {
		fprintf(stderr, "rbe-action-exec: usage: exec <n> <env...> <argv...>\n");
		return 125;
	}
	errno = 0;
	unsigned long n = strtoul(argv[1], &end, 10);
	if (errno || *end || argv[1][0] == '\0' || n > (unsigned long)(argc - 2) - 1) {
		fprintf(stderr, "rbe-action-exec: bad arguments\n");
		return 125;
	}
	char **env = calloc(n + 1, sizeof(char *));
	if (!env) return 125;
	for (unsigned long i = 0; i < n; i++) env[i] = argv[2 + i];
	char **cmd = &argv[2 + n];
	environ = env;
	execvp(cmd[0], cmd);
	int e = errno;
	fprintf(stderr, "rbe-action-exec: %s: %s\n", cmd[0], strerror(e));
	return e == ENOENT ? 127 : 126;
}
#else
int main(int argc, char **argv) {
	size_t n = 0;
	for (char **e = environ; *e; e++) n++;
	char **a = calloc((size_t)argc + n + 6, sizeof(char *));
	if (!a) return 125;
	char cnt[24];
	snprintf(cnt, sizeof cnt, "%zu", n);
	size_t i = 0;
	a[i++] = "sudo";
	a[i++] = "-n";
	a[i++] = "--";
	a[i++] = LAUNCH;
	a[i++] = cnt;
	for (char **e = environ; *e; e++) a[i++] = *e;
	for (int k = 1; k < argc; k++) a[i++] = argv[k];
	a[i] = NULL;
	char *clean[] = {"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C.UTF-8", NULL};
	execve(SUDO, a, clean);
	fprintf(stderr, "rbe-action-entry: exec %s: %s\n", SUDO, strerror(errno));
	return 125;
}
#endif
