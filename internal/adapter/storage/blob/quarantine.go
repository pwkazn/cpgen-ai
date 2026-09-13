package blob

// Quarantine is kept in its own file to make the corruption boundary obvious
// to reviewers. Corrupt canonical bytes are never overwritten; they are moved
// to the private quarantine directory before a replacement may be published.
