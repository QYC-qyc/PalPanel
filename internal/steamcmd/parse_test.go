package steamcmd

import "testing"

const fixtureManifest = `"AppState"
{
	"appid"		"2394010"
	"buildid"		"24575149"
}`

func TestParseManifestBuildID(t *testing.T) {
	if got := ParseManifestBuildID(fixtureManifest); got != "24575149" {
		t.Fatalf("got %q", got)
	}
	if got := ParseManifestBuildID("no match"); got != "" {
		t.Fatalf("want empty got %q", got)
	}
}

const fixtureAppInfo = `{
 "2394010"
 {
  "depots"
  {
   "branches"
   {
    "public"
    {
     "buildid"		"25080279"
     "timeupdated"		"1725800000"
    }
    "beta"
    {
     "buildid"		"12345678"
    }
   }
  }
 }
}`

func TestParseAppInfoBuildID(t *testing.T) {
	if got := ParseAppInfoBuildID(fixtureAppInfo); got != "25080279" {
		t.Fatalf("public branch: got %q", got)
	}
	if got := ParseAppInfoBuildID(`"buildid" "111" "buildid" "222"`); got != "111" {
		t.Fatalf("fallback first: got %q", got)
	}
}

func TestParseProgressLine(t *testing.T) {
	p, state, ok := ParseProgressLine(`Update state (0x61) downloading, progress: 45.6 (123456789 bytes).`)
	if !ok || p != 45 || state != "downloading" {
		t.Fatalf("p=%d state=%q ok=%v", p, state, ok)
	}
	if _, _, ok := ParseProgressLine("unrelated line"); ok {
		t.Fatal("unrelated should not match")
	}
}
