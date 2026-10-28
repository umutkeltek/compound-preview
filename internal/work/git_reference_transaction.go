package work

import "compound/internal/authority/gitrepository"

// Keep the work package's completion seam narrow and injectable while the
// authority package owns the Git implementation. Tests replace the starter to
// move refs at the last possible instant and prove the close path is CAS-safe.
type gitReferenceExpectation = gitrepository.ReferenceExpectation

type gitReferenceTransaction = gitrepository.ReferenceTransaction

var startGitReferenceTransaction = gitrepository.PrepareReferenceTransaction
