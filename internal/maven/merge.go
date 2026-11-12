package maven

import (
	"math/big"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// maxSnapshotDepth is the deepest snapshot document, in elements with the
// root counted, that the Python server writes out. ET.indent and the
// serializer of ElementTree recurse once per level and raise RecursionError
// at the limit of 1000 frames, the frames below the merge included.
// Measured on 2026-11-12 with the reference server behind a group: depth
// 987 gives 200, depth 988 gives 500, alike for GET, HEAD, .sha1 and .md5.
const maxSnapshotDepth = 987

// MergeMetadata is merge_metadata: one maven-metadata.xml from several.
// Documents that do not parse, or whose root is no metadata element, are
// skipped; with none left the result is nil, nil.
//
// Artifact level: the union of the versions in version order, the newest
// latest and release, the greatest lastUpdated, the ids of the first
// document that has them. Snapshot level (a versioning/snapshot element):
// the document with the greatest lastUpdated, whole. Group level: the union
// of the plugins by prefix, the first document wins.
//
// The errors are those Python raises: LookupError for an encoding Python
// does not know, ValueError from int() for a number it cannot read, and
// RecursionError for a snapshot document nested too deep.
func MergeMetadata(documents [][]byte) ([]byte, error) {
	return mergeMetadata(documents, pythonCodec)
}

func mergeMetadata(documents [][]byte, codecs func(string) codecResult) ([]byte, error) {
	var roots []*element
	for _, document := range documents {
		root, err := parseMetadata(document, codecs)
		if err != nil {
			return nil, err
		}
		if root != nil {
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}

	var snapshots []*element
	for _, root := range roots {
		if root.find("versioning", "snapshot") != nil {
			snapshots = append(snapshots, root)
		}
	}
	if len(snapshots) > 0 {
		newestRoot := snapshots[0]
		for _, root := range snapshots[1:] {
			candidate, err := timestamp(root.childText("versioning", "lastUpdated"))
			if err != nil {
				return nil, err
			}
			current, err := timestamp(newestRoot.childText("versioning", "lastUpdated"))
			if err != nil {
				return nil, err
			}
			if candidate.Cmp(current) > 0 {
				newestRoot = root
			}
		}
		if newestRoot.depth() > maxSnapshotDepth {
			return nil, &pyjson.Error{Class: "RecursionError", Message: "maximum recursion depth exceeded"}
		}
		return serialize(newestRoot), nil
	}

	merged := &element{tag: "metadata"}
	for _, root := range roots {
		if model := root.get("modelVersion"); model != "" {
			merged.attrs = append(merged.attrs, attr{"modelVersion", model})
			break
		}
	}
	for _, tag := range []string{"groupId", "artifactId", "version"} {
		for _, root := range roots {
			if value := root.childText(tag); value != "" {
				merged.addLeaf(tag, value)
				break
			}
		}
	}

	var versionings []*element
	for _, root := range roots {
		if node := root.find("versioning"); node != nil {
			versionings = append(versionings, node)
		}
	}
	if len(versionings) > 0 {
		var versions []string
		seen := map[string]bool{}
		for _, node := range versionings {
			for _, versionNode := range node.findAll("versions", "version") {
				value := pystr.Strip(versionNode.text)
				if value != "" && !seen[value] {
					seen[value] = true
					versions = append(versions, value)
				}
			}
		}
		if err := sortVersions(versions); err != nil {
			return nil, err
		}
		latest, err := newest(texts(versionings, "latest"))
		if err != nil {
			return nil, err
		}
		if latest == "" && len(versions) > 0 {
			latest = versions[len(versions)-1]
		}
		release, err := newest(texts(versionings, "release"))
		if err != nil {
			return nil, err
		}
		lastUpdated, err := greatestTimestamp(texts(versionings, "lastUpdated"))
		if err != nil {
			return nil, err
		}
		versioning := merged.addNode("versioning")
		versioning.addLeaf("latest", latest)
		versioning.addLeaf("release", release)
		if len(versions) > 0 {
			versionsNode := versioning.addNode("versions")
			for _, value := range versions {
				versionsNode.addLeaf("version", value)
			}
		}
		versioning.addLeaf("lastUpdated", lastUpdated)
	}

	var plugins []*element
	prefixes := map[string]bool{}
	for _, root := range roots {
		for _, plugin := range root.findAll("plugins", "plugin") {
			if prefix := plugin.childText("prefix"); prefix != "" && !prefixes[prefix] {
				prefixes[prefix] = true
				plugins = append(plugins, plugin)
			}
		}
	}
	if len(plugins) > 0 {
		pluginsNode := merged.addNode("plugins")
		for _, plugin := range plugins {
			copied := pluginsNode.addNode("plugin")
			for _, tag := range []string{"name", "prefix", "artifactId"} {
				copied.addLeaf(tag, plugin.childText(tag))
			}
		}
	}
	return serialize(merged), nil
}

// addNode is ET.SubElement.
func (e *element) addNode(tag string) *element {
	child := &element{tag: tag}
	e.children = append(e.children, child)
	return child
}

// addLeaf is _child of maven.py: a child with the text, only when the text
// is not None.
func (e *element) addLeaf(tag, text string) {
	if text != "" {
		e.children = append(e.children, &element{tag: tag, text: text})
	}
}

// texts are the non empty _text of tag in each node.
func texts(nodes []*element, tag string) []string {
	var out []string
	for _, node := range nodes {
		if value := node.childText(tag); value != "" {
			out = append(out, value)
		}
	}
	return out
}

var minusOne = big.NewInt(-1)

// timestamp is _timestamp: int(text) for a text of digits, -1 otherwise.
// str.isdigit counts digits int() refuses, such as a superscript two; for
// them int() raises ValueError, and so does this.
func timestamp(text string) (*big.Int, error) {
	if text == "" || !pyIsDigit(text) {
		return minusOne, nil
	}
	return pystr.ParseInt(text)
}

// greatestTimestamp is max(values, key=_timestamp): the first value with
// the greatest key; every key is computed, in order.
func greatestTimestamp(values []string) (string, error) {
	best, bestKey := "", (*big.Int)(nil)
	for _, value := range values {
		key, err := timestamp(value)
		if err != nil {
			return "", err
		}
		if bestKey == nil || key.Cmp(bestKey) > 0 {
			best, bestKey = value, key
		}
	}
	return best, nil
}
