// Which filesystem names does THIS kernel's mount(2) accept, and what does it answer?
//
// The registered-name question decides whether macOS 26 arm64 has a nullfs VFS that
// refuses on entitlement (EPERM) or no filesystem by that name at all (ENODEV). The two
// predictions are distinguishable by errno alone, so the probe asks each name three ways:
// with the conventional /dev/null mount data, with the data pointer nil, and with a spec
// built the way xnu's nullfs_mount() actually reads one -- a `struct null_mount_conf`
// followed by the source path, `copyin` then `copyinstr`, not a string. The third pass is
// what removes "malformed spec" as an explanation for EPERM on nullfs: a refusal that
// survives a correctly shaped spec is a policy refusal, not a parse failure.
//
// Errno MACROS are printed, never strerror text. Apple's bsd/sys/errno.h comments ENODEV
// (19) as "Operation not supported by device", so a probe that prints the string invites
// the reader to call 19 ENOTSUP, which on this OS is 45. Getting the name wrong here is how
// this conclusion swung twice on one node.
//
// libc mount()/unmount() are mount(2) and unmount(2) -- the same kernel entry the
// syscall(167) form reaches -- and they avoid the deprecated syscall() shim, which on
// arm64 macOS raises SIGSYS for some numbers and once killed this probe mid-run, after a
// successful mount, which is exactly the state change it must not leave behind. The mount
// table also reports only the resolved path, so never decide "not mounted" by comparing a
// statfs Mntonname against the path you passed: that once stranded six read-only devfs
// mounts on a node. Everything mounted here is released in reverse before exit.
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <unistd.h>

// xnu's nullfs.h and bindfs.h both take this: { uint64_t flags; } at offset 0 of the mount
// data, with the source path immediately after it.
struct null_mount_conf {
	uint64_t flags;
};

// Numbers, not macros, and deliberately so. On Darwin ENOTSUP is 45 while EOPNOTSUPP is
// either an alias of it or 102 depending on the feature-test macro, and on Linux the two are
// the same 95 -- so a `switch` on the macros cannot compile on both, and on Darwin it would
// silently merge two different names. The names below are transcribed from
// apple-oss-distributions/xnu bsd/sys/errno.h at xnu-12377.121.6, with the Linux number
// marked where the two OSes disagree.
static const char *errnoName(int e) {
	switch (e) {
	case 0:   return "none";
	case 1:   return "EPERM";
	case 2:   return "ENOENT";
	case 4:   return "EINTR";
	case 5:   return "EIO";
	case 6:   return "ENXIO";
	case 9:   return "EBADF";
	case 13:  return "EACCES";
	case 14:  return "EFAULT";
	case 16:  return "EBUSY";
	case 17:  return "EEXIST";
	case 18:  return "EXDEV";
	// 19 is ENODEV on BOTH OSes. Apple comments it "Operation not supported by device", so
	// strerror text invites the reader to call it ENOTSUP. It is not.
	case 19:  return "ENODEV (unregistered name -- NOT 'no such filesystem')";
	case 20:  return "ENOTDIR";
	case 21:  return "EISDIR";
	case 22:  return "EINVAL";
	case 28:  return "ENOSPC";
	case 30:  return "EROFS";
	case 45:  return "ENOTSUP (Darwin; Linux numbers it 95)";
	case 46:  return "EPFNOSUPPORT";
	case 47:  return "EAFNOSUPPORT";
	case 59:  return "ETOOMANYREFS";
	case 63:  return "ENAMETOOLONG";
	// 75 is Darwin's EPROGMISMATCH and Linux's EOVERFLOW, which is why an nfs refusal here
	// means different sentences on the two OSes.
	case 75:  return "EPROGMISMATCH on Darwin, EOVERFLOW on Linux";
	case 84:  return "EOVERFLOW (Darwin; Linux numbers it 75)";
	case 95:  return "ENOTSUP/EOPNOTSUPP on Linux";
	case 102: return "EOPNOTSUPP (Darwin, under the ANSI guard)";
	default:  return "other";
	}
}

int main(int argc, char **argv) {
	if (argc < 3) {
		fprintf(stderr, "usage: %s TARGET_ROOT TYPE [TYPE...]\n", argv[0]);
		return 2;
	}
	setvbuf(stdout, NULL, _IONBF, 0);
	const char *root = argv[1];
	// A spec-shaped mount data buffer: conf, then the path the alias would be drawn from.
	// The probe root is a real directory, so this is a well-formed request rather than a
	// deliberate garbage one.
	char spec[512];
	struct null_mount_conf *conf = (struct null_mount_conf *)spec;
	conf->flags = 0;
	strncpy(spec + sizeof(*conf), root, sizeof(spec) - sizeof(*conf) - 1);
	spec[sizeof(spec) - 1] = '\0';

	int mounted = 0;
	for (int i = 2; i < argc; i++) {
		char target[512];
		snprintf(target, sizeof(target), "%s/%s", root, argv[i]);
		if (mkdir(target, 0755) != 0 && errno != EEXIST) {
			fprintf(stderr, "mkdir %s: %s\n", target, strerror(errno));
			return 1;
		}
		for (int pass = 0; pass < 3; pass++) {
			void *data;
			const char *label;
			switch (pass) {
			case 0:
				data = (void *)"/dev/null";
				label = "string";
				break;
			case 1:
				data = NULL;
				label = "nil";
				break;
			default:
				data = (void *)spec;
				label = "conf+path";
				break;
			}
			errno = 0;
			int r = mount(argv[i], target, MNT_RDONLY, data);
			int e = errno;
			// The number first, always: the name above is a mapping this file owns, and a
			// reader who disagrees with it can still see what the kernel returned.
			printf("type=%-9s data=%-9s -> ret=%d errno=%d %s\n", argv[i], label, r, e,
			       errnoName(e));
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
	return mounted != 0;
}
