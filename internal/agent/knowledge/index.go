package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// IndexFile 是 knowledge/index.yaml（README §6）：各条目检索维度的汇总。
type IndexFile struct {
	Version string       `yaml:"version"`
	Entries []IndexEntry `yaml:"entries"`
}

// IndexEntry 是索引中的一行，检索维度须与条目 front-matter 完全一致。
type IndexEntry struct {
	KBID           string         `yaml:"kb_id"`
	File           string         `yaml:"file"`
	Version        string         `yaml:"version"`
	Crop           string         `yaml:"crop"`
	Stage          []string       `yaml:"stage"`
	Region         string         `yaml:"region"`
	Category       []string       `yaml:"category"`
	Topics         []string       `yaml:"topics"`
	Target         string         `yaml:"target,omitempty"`
	AuthorityLevel AuthorityLevel `yaml:"authority_level"`
	Deprecated     bool           `yaml:"deprecated,omitempty"`
}

func parseIndex(raw []byte) (*IndexFile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	idx := &IndexFile{}
	if err := dec.Decode(idx); err != nil {
		return nil, fmt.Errorf("index.yaml: %w", err)
	}
	if idx.Version == "" || len(idx.Entries) == 0 {
		return nil, errors.New("index.yaml: version and entries are required")
	}
	return idx, nil
}

// checkIndex 校验索引与条目双向一致：每个索引行对应一个条目且维度相同，每个条目都在索引中。
func checkIndex(idx *IndexFile, byID map[string]*Entry) error {
	var errs []error
	listed := make(map[string]bool, len(idx.Entries))
	for _, ie := range idx.Entries {
		if listed[ie.KBID] {
			errs = append(errs, fmt.Errorf("index.yaml: duplicate kb_id %q", ie.KBID))
			continue
		}
		listed[ie.KBID] = true
		e, ok := byID[ie.KBID]
		if !ok {
			errs = append(errs, fmt.Errorf("index.yaml: %s has no entry", ie.KBID))
			continue
		}
		for _, d := range ie.diff(e) {
			errs = append(errs, fmt.Errorf("index.yaml: %s %s differs from front-matter", ie.KBID, d))
		}
	}
	for id := range byID {
		if !listed[id] {
			errs = append(errs, fmt.Errorf("index.yaml: entry %s missing from index", id))
		}
	}
	return errors.Join(errs...)
}

func (ie IndexEntry) diff(e *Entry) []string {
	var out []string
	check := func(ok bool, field string) {
		if !ok {
			out = append(out, field)
		}
	}
	check(ie.File == e.File, "file")
	check(ie.Version == e.Version, "version")
	check(ie.Crop == e.Crop, "crop")
	check(slices.Equal(ie.Stage, e.Stage), "stage")
	check(ie.Region == e.Region, "region")
	check(slices.Equal(ie.Category, e.Category), "category")
	check(slices.Equal(ie.Topics, e.Topics), "topics")
	check(ie.Target == e.Target, "target")
	check(ie.AuthorityLevel == e.AuthorityLevel, "authority_level")
	return out
}
