package mount

import (
	"strings"
)

var virtualFilesystems = map[string]bool{
	"autofs": true, "cgroup": true, "cgroup2": true, "debugfs": true,
	"devpts": true, "devtmpfs": true, "fusectl": true, "hugetlbfs": true,
	"mqueue": true, "proc": true, "pstore": true, "securityfs": true,
	"sysfs": true, "tracefs": true, "tmpfs": true,
}

func unescape(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}
