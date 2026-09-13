package steamcmd

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const AppID = "2394010"

var buildidRe = regexp.MustCompile(`"buildid"\s+"(\d+)"`)
var progressRe = regexp.MustCompile(`Update state \(0x[0-9a-fA-F]+\) (\w+), progress: ([\d.]+)`)

// ParseManifestBuildID 从 appmanifest_*.acf 内容中解析 buildid。
func ParseManifestBuildID(content string) string {
	if m := buildidRe.FindStringSubmatch(content); m != nil {
		return m[1]
	}
	return ""
}

// ParseAppInfoBuildID 从 app_info 输出中解析 buildid，
// 优先取 "public" 分支下的值，否则回退到首个匹配。
func ParseAppInfoBuildID(output string) string {
	first, public := "", ""
	inPublic := false
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, `"public"`) {
			inPublic = true
		}
		if m := buildidRe.FindStringSubmatch(line); m != nil {
			if first == "" {
				first = m[1]
			}
			if inPublic && public == "" {
				public = m[1]
			}
		}
	}
	if public != "" {
		return public
	}
	return first
}

// ParseProgressLine 解析 SteamCMD 的 "Update state (0x..) xxx, progress: NN.N" 行。
func ParseProgressLine(line string) (int, string, bool) {
	m := progressRe.FindStringSubmatch(line)
	if m == nil {
		return 0, "", false
	}
	f, _ := strconv.ParseFloat(m[2], 64)
	return int(f), m[1], true
}

// ManifestBuildID 读取游戏目录下 steamapps/appmanifest_<AppID>.acf 并解析 buildid。
func ManifestBuildID(gameDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(gameDir, "steamapps", "appmanifest_"+AppID+".acf"))
	if err != nil {
		return "", err
	}
	id := ParseManifestBuildID(string(b))
	if id == "" {
		return "", errors.New("appmanifest 中未找到 buildid")
	}
	return id, nil
}
