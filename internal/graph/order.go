package graph

import (
	"container/heap"
	"slices"
)

// DeleteGroup is one strongly connected component (SCC) of the subgraph
// induced by the delete-target tables. Rows of all tables in a group are
// deleted as one unit; no order is guaranteed between them, so a cyclic
// group must be deleted with FK checks disabled (6.6).
type DeleteGroup struct {
	Tables []string // tables of the same SCC, sorted by name
	Cyclic bool     // the SCC has two or more tables, or its table references itself
}

// DeleteOrder computes the SCCs of the subgraph induced by tables and returns
// them so that a group holding a child table always comes before any group
// holding one of its parent tables (6.5).
//
// The induced subgraph contains only the edges whose ChildTable and
// ParentTable are both in tables. Duplicate names in tables are ignored.
// Names that no edge mentions (including names unknown to the graph) become
// non-cyclic singleton groups. Among groups with no order constraint between
// them, the group whose first (smallest) table name is smallest comes first,
// so the result does not depend on the order of tables. tables is not
// modified. An empty input gives an empty, non-nil slice.
func (g *Graph) DeleteOrder(tables []string) []DeleteGroup {
	names := slices.Clone(tables)
	slices.Sort(names)
	names = slices.Compact(names)
	n := len(names)
	index := make(map[string]int, n)
	for i, name := range names {
		index[name] = i
	}

	// adj[v] lists the parents of v (child -> parent), without duplicates.
	adj := make([][]int, n)
	selfLoop := make([]bool, n)
	for _, e := range g.edges {
		c, cok := index[e.ChildTable]
		p, pok := index[e.ParentTable]
		if !cok || !pok {
			continue
		}
		if c == p {
			selfLoop[c] = true
			continue
		}
		if !slices.Contains(adj[c], p) {
			adj[c] = append(adj[c], p)
		}
	}

	comp, ncomp := tarjan(adj)

	// Members of each component, in name order (names is sorted, so
	// appending in index order keeps them sorted).
	members := make([][]int, ncomp)
	for v := 0; v < n; v++ {
		members[comp[v]] = append(members[comp[v]], v)
	}

	// Condensation DAG: an edge from the child's component to the parent's
	// component. indeg counts distinct child components still pending.
	succ := make([][]int, ncomp)
	indeg := make([]int, ncomp)
	for v := 0; v < n; v++ {
		for _, p := range adj[v] {
			a, b := comp[v], comp[p]
			if a != b && !slices.Contains(succ[a], b) {
				succ[a] = append(succ[a], b)
				indeg[b]++
			}
		}
	}

	// Kahn's algorithm; ready components are taken by their smallest member
	// index, i.e. by their smallest table name.
	ready := &minHeap{}
	key := func(c int) int { return members[c][0] }
	byKey := make([]int, n) // smallest member index -> component
	for c := 0; c < ncomp; c++ {
		byKey[key(c)] = c
		if indeg[c] == 0 {
			heap.Push(ready, key(c))
		}
	}
	out := make([]DeleteGroup, 0, ncomp)
	for ready.Len() > 0 {
		c := byKey[heap.Pop(ready).(int)]
		group := DeleteGroup{Tables: make([]string, 0, len(members[c]))}
		for _, v := range members[c] {
			group.Tables = append(group.Tables, names[v])
		}
		group.Cyclic = len(members[c]) >= 2 || selfLoop[members[c][0]]
		out = append(out, group)
		for _, d := range succ[c] {
			if indeg[d]--; indeg[d] == 0 {
				heap.Push(ready, key(d))
			}
		}
	}
	return out
}

// tarjan finds the SCCs of the graph given by adj with an iterative version
// of Tarjan's algorithm. It returns each vertex's component number and the
// number of components.
func tarjan(adj [][]int) (comp []int, ncomp int) {
	n := len(adj)
	const unvisited = -1
	order := make([]int, n) // discovery index
	low := make([]int, n)
	onStack := make([]bool, n)
	comp = make([]int, n)
	for i := range order {
		order[i] = unvisited
	}
	var stack []int
	type frame struct{ v, next int } // next: index into adj[v] to visit next
	var call []frame
	counter := 0

	for root := 0; root < n; root++ {
		if order[root] != unvisited {
			continue
		}
		call = append(call, frame{root, 0})
		order[root], low[root] = counter, counter
		counter++
		stack = append(stack, root)
		onStack[root] = true

		for len(call) > 0 {
			top := &call[len(call)-1]
			v := top.v
			if top.next < len(adj[v]) {
				w := adj[v][top.next]
				top.next++
				switch {
				case order[w] == unvisited:
					order[w], low[w] = counter, counter
					counter++
					stack = append(stack, w)
					onStack[w] = true
					call = append(call, frame{w, 0})
				case onStack[w]:
					low[v] = min(low[v], order[w])
				}
				continue
			}
			// All successors of v are done.
			call = call[:len(call)-1]
			if len(call) > 0 {
				u := call[len(call)-1].v
				low[u] = min(low[u], low[v])
			}
			if low[v] == order[v] {
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					comp[w] = ncomp
					if w == v {
						break
					}
				}
				ncomp++
			}
		}
	}
	return comp, ncomp
}

// minHeap is a min-heap of ints for container/heap.
type minHeap []int

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *minHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
