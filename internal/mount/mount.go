package mount

import (
	"strings"
)

const (
	KindLocal     = "local"
	KindRemote    = "remote"
	KindContainer = "container"
	KindPseudo    = "pseudo"
	KindMemory    = "memory"
	KindUnknown   = "unknown"
)

type Info struct {
	Source   string `json:"source"`
	Path     string `json:"mount"`
	Type     string `json:"type"`
	Kind     string `json:"kind"`
	ReadOnly bool   `json:"read_only"`
	identity string
}

var pseudoFilesystems = map[string]bool{
	"autofs": true, "bpf": true, "cgroup": true, "cgroup2": true,
	"configfs": true, "debugfs": true, "devfs": true, "devpts": true,
	"efivarfs": true, "fuse.portal": true, "fusectl": true, "mqueue": true,
	"nsfs": true, "proc": true, "pstore": true, "rpc_pipefs": true,
	"securityfs": true, "sysfs": true, "tracefs": true,
}

var memoryFilesystems = map[string]bool{
	"devtmpfs": true, "hugetlbfs": true, "ramfs": true, "tmpfs": true,
}

var remoteFilesystems = map[string]bool{
	"9p": true, "afs": true, "ceph": true, "cifs": true, "davfs": true,
	"fuse.rclone": true, "fuse.s3fs": true, "fuse.sshfs": true,
	"gfs2": true, "glusterfs": true, "lustre": true, "nfs": true,
	"nfs4": true, "smbfs": true, "sshfs": true, "webdav": true,
}

var containerFilesystems = map[string]bool{
	"aufs": true, "fuse-overlayfs": true, "overlay": true,
}

var localFilesystems = map[string]bool{
	"apfs": true, "bcachefs": true, "btrfs": true, "exfat": true,
	"ext2": true, "ext3": true, "ext4": true, "f2fs": true,
	"hfs": true, "hfsplus": true, "iso9660": true, "jfs": true,
	"msdos": true, "nilfs2": true, "ntfs": true, "ntfs3": true,
	"reiserfs": true, "udf": true, "ufs": true, "vfat": true,
	"xfs": true, "zfs": true,
}

func Classify(filesystemType string) string {
	filesystemType = strings.ToLower(filesystemType)
	switch {
	case pseudoFilesystems[filesystemType]:
		return KindPseudo
	case memoryFilesystems[filesystemType]:
		return KindMemory
	case remoteFilesystems[filesystemType]:
		return KindRemote
	case containerFilesystems[filesystemType]:
		return KindContainer
	case localFilesystems[filesystemType]:
		return KindLocal
	default:
		return KindUnknown
	}
}

func unescape(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}

func hasOption(options, target string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == target {
			return true
		}
	}
	return false
}
