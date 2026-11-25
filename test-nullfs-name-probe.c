// Which filesystem names does THIS kernel's mount(2) accept, and what does it answer?
//
// The registered-name question decides whether macOS 26 arm64 has a nullfs VFS that
// refuses on entitlement (EPERM, macker's measurement) or no nullfs VFS at all (ENODEV,
// home-ops' reading). The two predictions are distinguishable by errno alone, so the probe
// asks each name twice: once with the conventional /dev/null mount data and once with the
// data pointer nil, which removes "malformed spec" as an explanation.
//
// libc mount()/unmount() are mount(2) and unmount(2) -- the same kernel entry the
// syscall(167) form reaches -- and they avoid the deprecated syscall() shim, which on
// arm64 macOS raises SIGSYS for some numbers and once killed this probe mid-run, after a
// successful mount, which is exactly the state change it must not leave behind.
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <unistd.h>

int main(int argc, char **argv) {
	if (argc < 3) {
		fprintf(stderr, "usage: %s TARGET_ROOT TYPE [TYPE...]\n", argv[0]);
		return 2;
	}
	setvbuf(stdout, NULL, _IONBF, 0);
	const char *root = argv[1];
	int mounted = 0;
	for (int i = 2; i < argc; i++) {
		char target[512];
		snprintf(target, sizeof(target), "%s/%s", root, argv[i]);
		if (mkdir(target, 0755) != 0 && errno != EEXIST) {
			fprintf(stderr, "mkdir %s: %s\n", target, strerror(errno));
			return 1;
		}
		for (int pass = 0; pass < 2; pass++) {
			void *data = pass == 0 ? (void *)"/dev/null" : NULL;
			errno = 0;
			int r = mount(argv[i], target, MNT_RDONLY, data);
			int e = errno;
			printf("type=%-9s data=%-9s -> ret=%d errno=%-3d %s\n", argv[i],
			       pass == 0 ? "/dev/null" : "nil", r, e, strerror(e));
			if (r == 0) {
				mounted++;
			}
		}
	}
	// Every mount this probe made is released before it exits, in reverse, so the node
	// keeps no trace of the question it was asked.
	for (int i = argc - 1; i >= 2 && mounted > 0; i--) {
		char target[512];
		snprintf(target, sizeof(target), "%s/%s", root, argv[i]);
		errno = 0;
		if (unmount(target, 0) == 0) {
			mounted--;
			printf("unmounted: %s\n", target);
		}
	}
	printf("mounts left: %d\n", mounted);
	return 0;
}
