// Package main generates winres.json for qzonewall-go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const js = `{
  "RT_GROUP_ICON": {
    "APP": {
      "0000": [
        "icon_256x256.png"
      ]
    }
  },
  "RT_MANIFEST": {
    "#1": {
      "0409": {
        "identity": {
          "name": "qzonewall-go",
          "version": "%s"
        },
        "description": "QzoneWall-Go submit and publish service",
        "minimum-os": "vista",
        "execution-level": "as invoker",
        "ui-access": false,
        "auto-elevate": false,
        "dpi-awareness": "system",
        "disable-theming": false,
        "disable-window-filtering": false,
        "high-resolution-scrolling-aware": false,
        "ultra-high-resolution-scrolling-aware": false,
        "long-path-aware": false,
        "printer-driver-isolation": false,
        "gdi-scaling": false,
        "segment-heap": false,
        "use-common-controls-v6": false
      }
    }
  },
  "RT_VERSION": {
    "#1": {
      "0000": {
        "fixed": {
          "file_version": "%s",
          "product_version": "%s",
          "timestamp": "%s"
        },
        "info": {
          "0409": {
            "Comments": "QzoneWall-Go service binary",
            "CompanyName": "guohuiyuan",
            "FileDescription": "https://github.com/guohuiyuan/qzonewall-go",
            "FileVersion": "%s",
            "InternalName": "qzonewall",
            "LegalCopyright": "%s",
            "LegalTrademarks": "",
            "OriginalFilename": "QZONEWALL.EXE",
            "PrivateBuild": "",
            "ProductName": "QzoneWall-Go",
            "ProductVersion": "%s",
            "SpecialBuild": ""
          }
        }
      }
    }
  }
}`

const timeformat = `2006-01-02T15:04:05+08:00`

func main() {
	f, err := os.Create("winres.json")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	version := "v1.0.0"
	commitcnt := strings.Builder{}
	commitcntcmd := exec.Command("git", "rev-list", "--count", "HEAD")
	commitcntcmd.Stdout = &commitcnt
	err = commitcntcmd.Run()

	fv := "1.0.0.0"
	if err == nil {
		commitCount := strings.TrimSpace(commitcnt.String())
		if commitCount != "" {
			fv = "1.0.0." + commitCount
		}
	}

	copyright := "(C) 2026 guohuiyuan. All Rights Reserved."
	_, err = fmt.Fprintf(f, js, fv, fv, version, time.Now().Format(timeformat), fv, copyright, version)
	if err != nil {
		panic(err)
	}
}
