/*
 * guest-init.c -- pid 1 for the per-action S2 microVM rootfs.
 *
 * Mirrors the contract of the S1 run-action-fc.sh prototype (design
 * rbe-microvm-single-pool-design.md 4.3) in miniature: mount the per-action
 * disk (vdb), run the action the host staged there (/run.sh, /env.txt) with
 * its stdout/stderr/exit code captured back onto the same disk, then power
 * the microVM off. No network device is ever attached (fork tier: always;
 * oss tier: this rig never turns one on either -- see report).
 *
 * Kept as a single static C binary (no libc dynamic deps, no busybox
 * dependency for the control flow) so the toolset rootfs has the smallest
 * possible device/attack surface, per 4.1/4.3 of the design. /bin/sh
 * (busybox) is what actually interprets run.sh; init only forks/execs it.
 */
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>
#include <linux/reboot.h>

static void die_reboot(const char *msg) {
	fprintf(stderr, "guest-init: %s\n", msg);
	fflush(stderr);
	sync();
	reboot(LINUX_REBOOT_CMD_RESTART);
	for (;;)
		pause();
}

/* Build an envp array from NUL-free, newline-separated KEY=VALUE lines in
 * /mnt/env.txt. Kept deliberately simple: the host-side launcher is in full
 * control of what it writes there (this is our own rig, not attacker input). */
static char **load_envp(const char *path) {
	FILE *f = fopen(path, "r");
	if (!f)
		return NULL;
	size_t cap = 64, n = 0;
	char **envp = malloc(cap * sizeof(char *));
	char *line = NULL;
	size_t linecap = 0;
	ssize_t len;
	while ((len = getline(&line, &linecap, f)) > 0) {
		if (line[len - 1] == '\n')
			line[len - 1] = '\0';
		if (line[0] == '\0')
			continue;
		if (n + 1 >= cap) {
			cap *= 2;
			envp = realloc(envp, cap * sizeof(char *));
		}
		envp[n++] = strdup(line);
	}
	envp[n] = NULL;
	free(line);
	fclose(f);
	return envp;
}

int main(void) {
	printf("MICROVM_GUEST_UP pid=%d uid=%d kernel-via-uname-in-run.sh\n", getpid(), getuid());
	fflush(stdout);

	/* No udev in this rootfs: the kernel's CONFIG_DEVTMPFS_MOUNT already
	 * auto-mounts devtmpfs at /dev before pid 1 runs on this kernel build;
	 * mount it ourselves only as a fallback (EBUSY if already mounted is
	 * fine, anything else that leaves /dev/vdb missing is fatal below). */
	mount("devtmpfs", "/dev", "devtmpfs", 0, NULL);

	if (mount("/dev/vdb", "/mnt", "ext4", 0, NULL) != 0)
		die_reboot("mount /dev/vdb failed");

	if (chdir("/mnt") != 0)
		die_reboot("chdir /mnt failed");

	if (access("/mnt/run.sh", F_OK) != 0)
		die_reboot("no /mnt/run.sh staged by host");

	int out_fd = open("/mnt/stdout.log", O_WRONLY | O_CREAT | O_TRUNC, 0644);
	int err_fd = open("/mnt/stderr.log", O_WRONLY | O_CREAT | O_TRUNC, 0644);
	if (out_fd < 0 || err_fd < 0)
		die_reboot("could not open stdout/stderr capture files");

	char **envp = load_envp("/mnt/env.txt");

	pid_t pid = fork();
	if (pid < 0)
		die_reboot("fork failed");

	if (pid == 0) {
		/* child: becomes the action process */
		dup2(out_fd, STDOUT_FILENO);
		dup2(err_fd, STDERR_FILENO);
		close(out_fd);
		close(err_fd);
		chdir("/mnt/work"); /* best-effort; run.sh also cds */
		char *argv[] = {"/bin/sh", "/mnt/run.sh", NULL};
		if (envp)
			execve("/bin/sh", argv, envp);
		else
			execv("/bin/sh", argv);
		/* execve only returns on error */
		_exit(127);
	}

	int status = 0;
	waitpid(pid, &status, 0);
	close(out_fd);
	close(err_fd);

	int rc = WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
	FILE *rcf = fopen("/mnt/exit_code", "w");
	if (rcf) {
		fprintf(rcf, "%d\n", rc);
		fclose(rcf);
	}

	sync();
	reboot(LINUX_REBOOT_CMD_RESTART);
	for (;;)
		pause();
	return 0;
}
