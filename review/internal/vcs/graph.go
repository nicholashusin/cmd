package vcs

import (
	"fmt"
	"html/template"
	"strings"
)

// GraphRow represents a rendered row in the commit graph.
type GraphRow struct {
	Change      *Change
	Lane        int
	NumLanes    int
	TopLanes    []int
	BottomEdges [][2]int
	IsCurrent   bool
}

// SVG returns safe SVG markup representing the graph lines and node for this row.
func (r GraphRow) SVG() template.HTML {
	numLanes := r.NumLanes
	if numLanes < 1 {
		numLanes = 1
	}
	width := numLanes * 18
	height := 36

	x := func(lane int) int {
		return lane*18 + 9
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="graph-svg" width="%d" height="%d" viewBox="0 0 %d %d" style="overflow: visible;" xmlns="http://www.w3.org/2000/svg">`, width, height, width, height)

	// Top incoming lines: from y=-1 to y=18 (bridges 1px border seamlessly)
	for _, l := range r.TopLanes {
		lx := x(l)
		fmt.Fprintf(&sb, `<line x1="%d" y1="-1" x2="%d" y2="18" stroke="var(--link)" stroke-width="2"/>`, lx, lx)
	}

	// Bottom outgoing edges: from y=18 to y=37 (bridges 1px border seamlessly)
	for _, edge := range r.BottomEdges {
		fromX := x(edge[0])
		toX := x(edge[1])
		if fromX == toX {
			fmt.Fprintf(&sb, `<line x1="%d" y1="18" x2="%d" y2="37" stroke="var(--link)" stroke-width="2"/>`, fromX, toX)
		} else {
			fmt.Fprintf(&sb, `<path d="M %d 18 C %d 27, %d 27, %d 37" stroke="var(--link)" stroke-width="2" fill="none"/>`, fromX, fromX, toX, toX)
		}
	}

	// Commit node at (x(r.Lane), 18)
	nodeX := x(r.Lane)
	if r.IsCurrent {
		fmt.Fprintf(&sb, `<circle cx="%d" cy="18" r="7" fill="var(--link)" opacity="0.3"/>`, nodeX)
		fmt.Fprintf(&sb, `<circle cx="%d" cy="18" r="4.5" fill="var(--link)"/>`, nodeX)
	} else if r.Change != nil && r.Change.Working {
		fmt.Fprintf(&sb, `<circle cx="%d" cy="18" r="4.5" fill="var(--bg)" stroke="var(--link)" stroke-width="2"/>`, nodeX)
	} else {
		fmt.Fprintf(&sb, `<circle cx="%d" cy="18" r="4.5" fill="var(--link)"/>`, nodeX)
	}

	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// BuildGraph arranges changes topologically and renders visual DAG graph lines,
// bridging parent-child relationships across filtered-out / hidden commits.
func BuildGraph(changes []*Change, visible func(*Change) bool) []GraphRow {
	if len(changes) == 0 {
		return nil
	}

	byRev := make(map[string]*Change)
	for _, c := range changes {
		byRev[c.Rev] = c
	}

	// Filter visible changes and bridge hidden parent links
	isVisible := func(c *Change) bool {
		if visible != nil {
			return visible(c)
		}
		return !c.Empty
	}

	findVisibleParent := func(rev string) string {
		curr := rev
		for curr != "" {
			c, ok := byRev[curr]
			if !ok {
				break
			}
			if isVisible(c) {
				return c.Rev
			}
			curr = c.Parent
		}
		return ""
	}

	type visibleNode struct {
		c          *Change
		parentRevs []string
	}

	var visibleNodes []visibleNode
	for _, c := range changes {
		if !isVisible(c) {
			continue
		}
		var parents []string
		if c.Parent != "" {
			vp := findVisibleParent(c.Parent)
			if vp != "" {
				parents = append(parents, vp)
			}
		}
		visibleNodes = append(visibleNodes, visibleNode{c: c, parentRevs: parents})
	}

	if len(visibleNodes) == 0 {
		return nil
	}

	var activeLanes []string
	var rows []GraphRow

	for _, node := range visibleNodes {
		c := node.c
		nodeRev := c.Rev

		// Incoming active lanes for this row
		var topLanes []int
		for i, l := range activeLanes {
			if l != "" {
				topLanes = append(topLanes, i)
			}
		}

		// Find or assign lane for this commit
		laneIdx := -1
		for i, l := range activeLanes {
			if l == nodeRev {
				laneIdx = i
				break
			}
		}
		if laneIdx == -1 {
			// Find first empty slot or append
			for i, l := range activeLanes {
				if l == "" {
					laneIdx = i
					activeLanes[i] = nodeRev
					break
				}
			}
			if laneIdx == -1 {
				laneIdx = len(activeLanes)
				activeLanes = append(activeLanes, nodeRev)
			}
		}

		nextLanes := make([]string, len(activeLanes))
		copy(nextLanes, activeLanes)

		var bottomEdges [][2]int

		// Pass-through lanes for other commits
		for i, l := range activeLanes {
			if i != laneIdx && l != "" {
				bottomEdges = append(bottomEdges, [2]int{i, i})
			}
		}

		// Update lanes for parents of this commit
		if len(node.parentRevs) == 0 {
			nextLanes[laneIdx] = ""
		} else {
			p0 := node.parentRevs[0]
			// Check if p0 is already in another lane
			targetLane := -1
			for i, l := range activeLanes {
				if i != laneIdx && l == p0 {
					targetLane = i
					break
				}
			}
			if targetLane != -1 {
				// Merging into existing lane
				nextLanes[laneIdx] = ""
				bottomEdges = append(bottomEdges, [2]int{laneIdx, targetLane})
			} else {
				nextLanes[laneIdx] = p0
				bottomEdges = append(bottomEdges, [2]int{laneIdx, laneIdx})
			}

			// Additional parents
			for _, pOther := range node.parentRevs[1:] {
				otherTarget := -1
				for i, l := range activeLanes {
					if l == pOther {
						otherTarget = i
						break
					}
				}
				if otherTarget != -1 {
					bottomEdges = append(bottomEdges, [2]int{laneIdx, otherTarget})
				} else {
					// Allocate new lane
					newLane := -1
					for i, l := range nextLanes {
						if l == "" {
							newLane = i
							nextLanes[i] = pOther
							break
						}
					}
					if newLane == -1 {
						newLane = len(nextLanes)
						nextLanes = append(nextLanes, pOther)
					}
					bottomEdges = append(bottomEdges, [2]int{laneIdx, newLane})
				}
			}
		}

		// Trim trailing empty lanes in nextLanes
		for len(nextLanes) > 0 && nextLanes[len(nextLanes)-1] == "" {
			nextLanes = nextLanes[:len(nextLanes)-1]
		}

		numLanes := len(activeLanes)
		if len(nextLanes) > numLanes {
			numLanes = len(nextLanes)
		}
		if laneIdx+1 > numLanes {
			numLanes = laneIdx + 1
		}

		rows = append(rows, GraphRow{
			Change:      c,
			Lane:        laneIdx,
			NumLanes:    numLanes,
			TopLanes:    topLanes,
			BottomEdges: bottomEdges,
		})

		activeLanes = nextLanes
	}

	return rows
}
