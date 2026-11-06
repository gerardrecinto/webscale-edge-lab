//go:build linux

package hostdoctor

import (
	"bufio"
	"os"
	"strings"
	"syscall"
)

var realFS = map[string]bool{"ext4": true, "ext3": true, "xfs": true, "btrfs": true, "f2fs": true, "zfs": true, "overlay": true}

func filesystems(mounts string) []FSUsage {
	file, err := os.Open(mounts)
	if err != nil {
		return nil
	}
	defer file.Close()
	seen := map[string]bool{}
	var out []FSUsage
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 3 || !realFS[p[2]] || seen[p[0]+p[1]] {
			continue
		}
		seen[p[0]+p[1]] = true
		var st syscall.Statfs_t
		if err := syscall.Statfs(p[1], &st); err != nil || st.Blocks == 0 {
			continue
		}
		u := FSUsage{Mount: p[1], Type: p[2], TotalBytes: st.Blocks * uint64(st.Bsize)}
		// Used is measured against what a normal user can use (Bavail), which is what df shows.
		used := st.Blocks - st.Bfree
		u.UsedPct = 100 * float64(used) / float64(used+st.Bavail)
		if st.Files > 0 {
			u.InodesPct = 100 * float64(st.Files-st.Ffree) / float64(st.Files)
		}
		out = append(out, u)
	}
	return out
}
