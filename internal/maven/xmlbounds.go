package maven

// Bounds of one metadata document beyond the rules of expat. A document over
// a bound is not well-formed, so a group answers from its other members.
const (
	// maxExpansion is the most bytes the internal entities of one document
	// may expand to, the size at which the amplification rule starts.
	maxExpansion = amplificationThreshold
	// maxElementDepth is the deepest element nesting of one document. It is
	// the depth of the deepest committed vector (snapshot_nested_1100), which
	// must parse to give the RecursionError of the reference.
	maxElementDepth = 1102
)

// Bounds of the tree of one document: elements and attributes in all,
// attribute defaults of the internal subset included.
const (
	maxElements   = 200_000
	maxAttributes = 200_000
)

// treeCount counts the elements and attributes of one document.
type treeCount struct {
	elements, attributes int
}

// add counts one element with its attributes.
func (c *treeCount) add(attributes int) error {
	c.elements++
	c.attributes += attributes
	if c.elements > maxElements || c.attributes > maxAttributes {
		return errNotWellFormed
	}
	return nil
}
