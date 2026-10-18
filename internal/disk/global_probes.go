package disk

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (e *Engine) readGlobalUse(ctx context.Context) globalUse {
	if e.globalUse != nil {
		return e.globalUse(ctx)
	}
	u := globalUse{activity: e.Activity(ctx)}
	b, err := globalProbe(ctx, "/bin/ps", "-U", strconv.Itoa(os.Getuid()), "-o", "comm=")
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				u.processes = append(u.processes, filepath.Base(line))
			}
		}
		u.processesKnown = len(u.processes) > 0
	}
	b, err = globalProbe(ctx, "/usr/bin/hdiutil", "info", "-plist")
	if err == nil {
		u.mounts, _ = parseGlobalMounts(b)
	}
	return u
}

func globalProbe(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := &cappedBuffer{limit: 1 << 20}
	diagnostics := &cappedBuffer{limit: 64 << 10}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = out
	cmd.Stderr = diagnostics
	if err := cmd.Run(); err != nil || out.overflow || diagnostics.Len() != 0 || diagnostics.overflow {
		return nil, errors.New("global activity probe unavailable")
	}
	return out.Bytes(), nil
}

type globalPlistNode struct {
	XMLName  xml.Name
	Text     string            `xml:",chardata"`
	Children []globalPlistNode `xml:",any"`
}

func globalPlistDict(n globalPlistNode) (map[string]globalPlistNode, error) {
	if n.XMLName.Local != "dict" || len(n.Children)%2 != 0 {
		return nil, errors.New("invalid mounted-image dictionary")
	}
	d := map[string]globalPlistNode{}
	for i := 0; i < len(n.Children); i += 2 {
		k := n.Children[i]
		if k.XMLName.Local != "key" || k.Text == "" {
			return nil, errors.New("invalid mounted-image key")
		}
		if _, ok := d[k.Text]; ok {
			return nil, errors.New("duplicate mounted-image key")
		}
		d[k.Text] = n.Children[i+1]
	}
	return d, nil
}

func parseGlobalMounts(b []byte) (Activity, error) {
	var n globalPlistNode
	decoder := xml.NewDecoder(bytes.NewReader(b))
	if len(b) > 1<<20 || decoder.Decode(&n) != nil || n.XMLName.Local != "plist" || len(n.Children) != 1 {
		return Activity{}, errors.New("invalid mounted-image inventory")
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		text, ok := token.(xml.CharData)
		if err != nil || !ok || strings.TrimSpace(string(text)) != "" {
			return Activity{}, errors.New("trailing mounted-image inventory data")
		}
	}
	d, err := globalPlistDict(n.Children[0])
	if err != nil {
		return Activity{}, err
	}
	images, ok := d["images"]
	if !ok || images.XMLName.Local != "array" {
		return Activity{}, errors.New("mounted-image inventory incomplete")
	}
	a := Activity{Known: true}
	for _, image := range images.Children {
		d, err := globalPlistDict(image)
		if err != nil {
			return Activity{}, err
		}
		p, ok := d["image-path"]
		if !ok || p.XMLName.Local != "string" || !filepath.IsAbs(p.Text) {
			return Activity{}, errors.New("mounted-image path unknown")
		}
		a.Paths = append(a.Paths, filepath.Clean(p.Text))
		if canonical, err := filepath.EvalSymlinks(p.Text); err == nil && canonical != p.Text {
			a.Paths = append(a.Paths, canonical)
		}
	}
	return a, nil
}
