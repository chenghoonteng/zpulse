// zfsio.c attributes ZFS read/write bytes to the process that caused them.
//
// zpulse's other data sources (zpool/zfs output, ZFS kstats) are all aggregate:
// they can tell you a dataset is busy but never who is making it busy. This
// program fills that gap by counting bytes at the VFS layer, keyed by
// (thread group id, dataset device id).
//
// Hook choice: the four generic VFS entry points below are in-tree kernel
// functions with signatures that have been stable for years. Hooking ZFS's own
// module symbols instead would be a smaller program, but ZFS is out-of-tree and
// renames internal symbols between releases; that trade is deliberately not
// taken here. The cost is that the two paths that do not pass through these
// functions -- mmap'd writeback and splice/sendfile -- are not counted.
//
// NOTE ON LICENSING: this file is GPL-licensed, unlike the rest of zpulse
// (MIT). bpf_probe_read_kernel() and friends are GPL-only helpers, so the
// license string below is not optional. Only this .c file and the object
// compiled from it are GPL; the Go program that loads it is not a derived work
// of it any more than a program is of the kernel it calls.

// linux/bpf.h supplies the map-type and flag enums; the kernel structs this
// program touches are declared by hand below instead of pulling in a generated
// vmlinux.h, which would be several megabytes of header for three fields.
#include <linux/types.h>
#include <linux/bpf.h>

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

// ZFS's superblock magic, from include/os/linux/zfs/sys/zfs_vfsops.h. It is not
// in the kernel's magic.h because ZFS is not in the kernel tree.
#define ZFS_SUPER_MAGIC 0x2fc12fc1

// Only the fields actually read are declared. preserve_access_index makes clang
// emit CO-RE relocations for them, so member offsets are resolved against the
// running kernel's BTF at load time rather than baked in at compile time. That
// is what lets one object file work across kernel versions.
struct super_block {
	unsigned long s_magic;
	__u32 s_dev;
} __attribute__((preserve_access_index));

struct inode {
	struct super_block *i_sb;
} __attribute__((preserve_access_index));

struct file {
	struct inode *f_inode;
} __attribute__((preserve_access_index));

// io_key identifies one process's traffic against one dataset. The dev field is
// the superblock's dev_t; userspace maps it back to a dataset name via
// /proc/self/mountinfo, which prints the same value as major:minor.
struct io_key {
	__u32 tgid;
	__u32 dev;
};

// io_stats are monotonic counters. Userspace diffs successive reads of the map
// to produce rates, so nothing here is ever reset from this side.
struct io_stats {
	__u64 read_bytes;
	__u64 write_bytes;
	__u64 read_ops;
	__u64 write_ops;
	char comm[16];
};

// Sized for a busy host: one entry per (process, dataset) pair with live I/O.
// Userspace prunes entries for dead processes, so this is a high-water mark for
// concurrent I/O-doing processes, not for uptime.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, struct io_key);
	__type(value, struct io_stats);
	__uint(max_entries, 16384);
} zfs_io SEC(".maps");

static __always_inline void record(struct file *file, long ret, int is_write)
{
	// Short reads are normal; errors and EOF are not I/O. Either way only a
	// positive return moved bytes.
	if (ret <= 0)
		return;

	struct super_block *sb = BPF_CORE_READ(file, f_inode, i_sb);
	if (!sb)
		return;

	// The filter that makes this cheap for everyone else: every read() and
	// write() on the host runs this program, and non-ZFS ones leave here.
	if (BPF_CORE_READ(sb, s_magic) != ZFS_SUPER_MAGIC)
		return;

	struct io_key key = {
		.tgid = bpf_get_current_pid_tgid() >> 32,
		.dev = BPF_CORE_READ(sb, s_dev),
	};

	struct io_stats *st = bpf_map_lookup_elem(&zfs_io, &key);
	if (!st) {
		// comm is captured once, at first sight of the process, because
		// by the time userspace reads the map the process may be gone
		// and /proc/<pid> with it.
		struct io_stats init = {};
		bpf_get_current_comm(&init.comm, sizeof(init.comm));
		bpf_map_update_elem(&zfs_io, &key, &init, BPF_NOEXIST);

		st = bpf_map_lookup_elem(&zfs_io, &key);
		if (!st)
			return; // map full
	}

	// Atomic because the same (pid, dataset) pair can be updated from several
	// CPUs at once; a shared hash map has no per-CPU split to hide behind.
	if (is_write) {
		__sync_fetch_and_add(&st->write_bytes, ret);
		__sync_fetch_and_add(&st->write_ops, 1);
	} else {
		__sync_fetch_and_add(&st->read_bytes, ret);
		__sync_fetch_and_add(&st->read_ops, 1);
	}
}

// fexit rather than kprobe+kretprobe: the return value is the byte count, and
// fexit gives it alongside the arguments in a single program with no scratch map
// and no kretprobe maxactive limit. It requires a BTF-equipped kernel >= 5.5.
//
// vfs_read/vfs_write cover read, write, pread and pwrite; vfs_readv/vfs_writev
// cover the iovec forms used by qemu and other AIO-style callers. The paths do
// not nest, so a single operation is never counted twice.

SEC("fexit/vfs_read")
int BPF_PROG(zfs_vfs_read, struct file *file, char *buf, __u64 count,
	     __u64 *pos, long ret)
{
	record(file, ret, 0);
	return 0;
}

SEC("fexit/vfs_write")
int BPF_PROG(zfs_vfs_write, struct file *file, const char *buf, __u64 count,
	     __u64 *pos, long ret)
{
	record(file, ret, 1);
	return 0;
}

SEC("fexit/vfs_readv")
int BPF_PROG(zfs_vfs_readv, struct file *file, const void *vec, __u64 vlen,
	     __u64 *pos, __u32 flags, long ret)
{
	record(file, ret, 0);
	return 0;
}

SEC("fexit/vfs_writev")
int BPF_PROG(zfs_vfs_writev, struct file *file, const void *vec, __u64 vlen,
	     __u64 *pos, __u32 flags, long ret)
{
	record(file, ret, 1);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
