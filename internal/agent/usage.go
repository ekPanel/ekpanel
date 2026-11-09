package agent

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"ekpanel/internal/agentproto"
)

// dirBytes adds up the space allocated to everything under root, without following links
// or crossing into other file systems. Each hard-linked file counts once.
func dirBytes(ctx context.Context, root string) int64 {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return 0
	}
	rootDev := uint64(0)
	if st, ok := rootInfo.Sys().(*syscall.Stat_t); ok {
		rootDev = uint64(st.Dev)
	}
	type inode struct{ dev, ino uint64 }
	seen := map[inode]bool{}
	var total int64
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			total += info.Size()
			return nil
		}
		if uint64(st.Dev) != rootDev {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if st.Nlink > 1 && !d.IsDir() {
			k := inode{uint64(st.Dev), uint64(st.Ino)}
			if seen[k] {
				return nil
			}
			seen[k] = true
		}
		total += st.Blocks * 512
		return nil
	})
	return total
}

func diskUsage(ctx context.Context, sys *system, p agentproto.DiskUsageParams) (*agentproto.DiskUsageResult, *agentproto.OpError) {
	res := &agentproto.DiskUsageResult{Sites: map[string]int64{}, Apps: map[string]int64{}}
	for _, s := range p.Sites {
		if !uuidRE.MatchString(s.ID) || !userRE.MatchString(s.User) || slices.Contains(reserved, s.User) {
			return nil, opError(agentproto.ErrInvalidParams, "invalid site %q", s.ID)
		}
		res.Sites[s.ID] = dirBytes(ctx, sys.path("/home/"+s.User))
	}
	for _, a := range p.Apps {
		if !uuidRE.MatchString(a.ID) || !appNameRE.MatchString(a.Name) {
			return nil, opError(agentproto.ErrInvalidParams, "invalid app %q", a.ID)
		}
		st, err := readAppState(sys, a.Name)
		if err != nil || st.ID != a.ID {
			continue // not set up yet, or another app's folder
		}
		total := dirBytes(ctx, sys.path(appDir(a.Name)))
		for _, mount := range volumeMounts(ctx, sys, a.Name) {
			total += dirBytes(ctx, mount)
		}
		res.Apps[a.ID] = total
	}
	return res, nil
}

// volumeMounts lists where Docker keeps the volumes of an app's Compose project.
func volumeMounts(ctx context.Context, sys *system, name string) []string {
	out, err := sys.run(ctx, "docker", "volume", "ls", "--quiet", "--filter", "label=com.docker.compose.project="+appProject(name))
	if err != nil {
		return nil
	}
	vols := strings.Fields(string(out))
	if len(vols) == 0 {
		return nil
	}
	out, err = sys.run(ctx, "docker", append([]string{"volume", "inspect"}, vols...)...)
	if err != nil {
		return nil
	}
	var info []struct {
		Mountpoint string `json:"Mountpoint"`
	}
	if json.Unmarshal(out, &info) != nil {
		return nil
	}
	var mounts []string
	for _, v := range info {
		// Only Docker's own volume area; a volume driver could point anywhere.
		if strings.HasPrefix(v.Mountpoint, "/var/lib/docker/volumes/") {
			mounts = append(mounts, sys.path(v.Mountpoint))
		}
	}
	return mounts
}
