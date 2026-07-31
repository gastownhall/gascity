// Package cityparity is the cross-adapter certification for Gas City's three
// neutral producer adapters: cityneutral (run, session, transcript),
// cityartifact (artifact and links) and cityinference (model invocations).
//
// It owns no product code and adds no behavior. It exists because the three
// adapters are three independent implementations of one contract, and three
// implementations can diverge. Two properties are worth proving across them and
// cannot be proven inside any one of them:
//
//   - Parity. A custom producer with no City anywhere in the path uploads the
//     same work and gets resources of the same public shape: the same kinds of
//     Team ID, the same links, the same provenance meanings, the same coverage
//     claims, the same content boundaries and the same retrieval behavior. If
//     City parity needed a City-shaped field, City would have stopped being
//     optional.
//
//   - Fault-domain independence. One adapter faulting, lagging, rolling back,
//     resetting its source epoch or rotating its credential affects that
//     adapter's checkpoint and nothing else. Acknowledged records survive, and
//     the other two adapters keep running.
//
// Where the three adapters do NOT agree, the tests here say so by name rather
// than by silence. A divergence recorded as a passing characterization test is
// a certification finding: it is what blocks the parity report, and the test
// name is the citation.
package cityparity
