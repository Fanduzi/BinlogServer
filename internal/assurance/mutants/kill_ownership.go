//go:build ignore

// Package main provides module-level functionality for main.
// input: the mutant table in kill.go
// output: mutantsOwnership appended to mutants in init order
// pos: ADR 0011 PR-4 claim and stop guard mutants, split from kill.go
// note: if this file changes, update this header and module README.md.
package main

var mutantsOwnership = []mutant{
	// ADR 0011 PR-4 claim and stop guards.
	{id: "P401", desc: "a stored refusal does not block Acquire", file: ownFile,
		old: "\tif err := s.refuseStoredClaimLocked(id); err != nil {\n\t\treturn Task{}, err\n\t}\n",
		new: ""},
	{id: "P402", desc: "an in-memory refusal does not block Acquire", file: ownFile,
		old: "\tif s.refusalBlocksLocked(id, s.clusterWorkerID, time.Now()) {\n\t\treturn Task{}, ErrLeaseNotAcquired\n\t}\n",
		new: ""},
	{id: "P403", desc: "an ownerless Start still takes the lease", file: stepFile,
		old: "\tif e.Ownerless {\n\t\treturn ownerlessStart(s, e, now)\n\t}\n",
		new: ""},
	{id: "P404", desc: "the terminal allowlist never reaches FAILED", file: ownFile,
		old: "\tif s.terminalRetry <= 0 {\n\t\treturn false\n\t}\n",
		new: "\treturn true\n"},
	{id: "P405", desc: "an older live protocol does not refuse the claim", file: ownFile,
		old: "\tif older > 0 {\n",
		new: "\tif false {\n"},
	{id: "P406", desc: "a storage mismatch does not refuse the claim", file: ownFile,
		old: "\tif registered != \"\" && registered != mode {\n\t\treturn fmt.Errorf(\"cluster.storage %s does not match registered %s\", mode, registered)\n\t}\n",
		new: ""},
	{id: "P407", desc: "StopExpired still sees a live lease", file: stopFile,
		old: "\tif st.Lease.Owner != \"\" && !st.Lease.ExpireAt.Before(now) {\n\t\tst.Lease.ExpireAt = now.Add(-time.Nanosecond)\n\t}\n",
		new: ""},
}

func init() {
	mutants = append(mutants, mutantsOwnership...)
}
