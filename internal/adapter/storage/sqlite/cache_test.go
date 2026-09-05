package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestCacheMutationMigrationCreatesLedgerTables(t *testing.T) {
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "cache.db"), clock.NewFake(testNow))
	defer store.Close()
	rows, err := store.db.QueryContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name IN
		('cache_entries','cache_entry_sources','cache_blob_refs','cache_blob_order','mutation_accounts','mutation_stage_accounts','mutation_claims','mutation_intents','mutation_records','mutation_record_operations','mutation_record_reservations','mutation_record_output_occurrences') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var count int
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 12 {
		t.Fatalf("ledger table count = %d, want 12", count)
	}
}

func TestCacheLookupRejectsMismatchedPolicyAndInput(t *testing.T) {
	key := domain.CacheKey{Digest: domain.SumBytes([]byte("key")), Kind: "statement"}
	entry := domain.CacheEntry{Key: key, Kind: key.Kind, SchemaVersion: "cpgen.cache/v1", PolicyDigest: domain.SumBytes([]byte("policy")), InputDigest: domain.SumBytes([]byte("input")), State: domain.CacheEntryValid, CreatedAt: testNow, Source: domain.CacheSource{RunID: testRunID, CallRecordID: "callrec_00000000000000000000000000000001", OccurrenceID: "occurrence_00000000000000000000000000000001", Digest: domain.SumBytes([]byte("input"))}, Blobs: []domain.CacheBlob{{Blob: domain.BlobRef{Digest: domain.SumBytes([]byte("input")), Size: 5}, Role: domain.ArtifactOutput, MediaType: "text/plain", LogicalPath: "output", Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}}}}
	lookup := domain.CacheLookup{RunID: testRunID, Key: key, SchemaVersion: entry.SchemaVersion, PolicyDigest: entry.PolicyDigest, InputDigest: domain.SumBytes([]byte("different")), At: testNow}
	candidate := domain.CacheCandidate{Entry: entry, SourceCallRecordID: entry.Source.CallRecordID, SourceOccurrenceIDs: []domain.ArtifactOccurrenceID{entry.Source.OccurrenceID}}
	if err := candidate.ValidateFor(lookup); err == nil {
		t.Fatal("mismatched cache input was accepted")
	}
}

func TestReadCacheCandidateRejectsProvenanceDigestTampering(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE cache_entry_sources (cache_key_digest TEXT, source_run_id TEXT, source_call_record_id TEXT, source_occurrence_id TEXT, source_digest TEXT)`,
		`CREATE TABLE cache_blob_refs (cache_key_digest TEXT, digest TEXT, size INTEGER, role TEXT, media_type TEXT, logical_path TEXT, provenance_json BLOB, provenance_digest TEXT)`,
		`CREATE TABLE cache_blob_order (cache_key_digest TEXT, blob_ordinal INTEGER, digest TEXT, size INTEGER, role TEXT, source_occurrence_id TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	key := domain.CacheKey{Digest: domain.SumBytes([]byte("tamper-key")), Kind: "statement"}
	blob := domain.SumBytes([]byte("tamper-blob"))
	run := domain.RunID("run_00000000000000000000000000000001")
	call := domain.CallRecordID("callrec_00000000000000000000000000000001")
	occurrence := domain.ArtifactOccurrenceID("occurrence_00000000000000000000000000000001")
	provenance := domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}
	payload, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cache_entry_sources VALUES (?, ?, ?, ?, ?)`, key.Digest, run, call, occurrence, blob); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cache_blob_refs VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, key.Digest, blob, 11, domain.ArtifactOutput, "text/plain", "out", payload, domain.SumBytes(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cache_blob_order VALUES (?, ?, ?, ?, ?, ?)`, key.Digest, 1, blob, 11, domain.ArtifactOutput, occurrence); err != nil {
		t.Fatal(err)
	}
	entry := domain.CacheEntry{Key: key, Kind: key.Kind, SchemaVersion: "cpgen.cache/v1", PolicyDigest: domain.SumBytes([]byte("policy")), InputDigest: domain.SumBytes([]byte("input")), State: domain.CacheEntryValid, CreatedAt: testNow}
	if _, err := readCacheCandidate(context.Background(), db, entry); err != nil {
		t.Fatalf("untampered cache candidate: %v", err)
	}
	tampered := []byte(`{"schema_version":"cpgen.artifact/v1","producer":"attacker"}`)
	if _, err := db.Exec(`UPDATE cache_blob_refs SET provenance_json = ? WHERE cache_key_digest = ?`, tampered, key.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := readCacheCandidate(context.Background(), db, entry); err == nil {
		t.Fatal("tampered provenance was accepted")
	}
}

func TestReadCacheCandidatePreservesDurableBlobOrderAcrossRandomOccurrenceIDs(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE cache_entry_sources (cache_key_digest TEXT, source_run_id TEXT, source_call_record_id TEXT, source_occurrence_id TEXT, source_digest TEXT)`,
		`CREATE TABLE cache_blob_refs (cache_key_digest TEXT, digest TEXT, size INTEGER, role TEXT, media_type TEXT, logical_path TEXT, provenance_json BLOB, provenance_digest TEXT)`,
		`CREATE TABLE cache_blob_order (cache_key_digest TEXT, blob_ordinal INTEGER, digest TEXT, size INTEGER, role TEXT, source_occurrence_id TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	key := domain.CacheKey{Digest: domain.SumBytes([]byte("order-key")), Kind: "statement"}
	run := domain.RunID("run_00000000000000000000000000000021")
	call := domain.CallRecordID("callrec_00000000000000000000000000000021")
	// The IDs intentionally sort opposite to the durable blob ordinals.
	firstOccurrence := domain.ArtifactOccurrenceID("occurrence_ffffffffffffffffffffffffffffffff")
	secondOccurrence := domain.ArtifactOccurrenceID("occurrence_00000000000000000000000000000000")
	firstData := []byte("first output")
	secondData := []byte("second output")
	firstDigest, secondDigest := domain.SumBytes(firstData), domain.SumBytes(secondData)
	provenance := domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "order-test"}
	payload, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []struct {
		occurrence domain.ArtifactOccurrenceID
		digest     domain.Digest
	}{
		{firstOccurrence, firstDigest}, {secondOccurrence, secondDigest},
	} {
		if _, err := db.Exec(`INSERT INTO cache_entry_sources VALUES (?, ?, ?, ?, ?)`, key.Digest, run, call, source.occurrence, source.digest); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		digest domain.Digest
		size   int
		role   domain.ArtifactRole
		path   string
	}{
		{firstDigest, len(firstData), domain.ArtifactOutput, "first"},
		{secondDigest, len(secondData), domain.ArtifactStdout, "second"},
	} {
		if _, err := db.Exec(`INSERT INTO cache_blob_refs VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, key.Digest, item.digest, item.size, item.role, "text/plain", item.path, payload, domain.SumBytes(payload)); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range []struct {
		ordinal    int
		digest     domain.Digest
		size       int
		role       domain.ArtifactRole
		occurrence domain.ArtifactOccurrenceID
	}{
		{1, firstDigest, len(firstData), domain.ArtifactOutput, firstOccurrence},
		{2, secondDigest, len(secondData), domain.ArtifactStdout, secondOccurrence},
	} {
		if _, err := db.Exec(`INSERT INTO cache_blob_order VALUES (?, ?, ?, ?, ?, ?)`, key.Digest, order.ordinal, order.digest, order.size, order.role, order.occurrence); err != nil {
			t.Fatal(err)
		}
	}
	entry := domain.CacheEntry{Key: key, Kind: key.Kind, SchemaVersion: "cpgen.cache/v1", PolicyDigest: domain.SumBytes([]byte("policy")), InputDigest: domain.SumBytes([]byte("input")), State: domain.CacheEntryValid, CreatedAt: testNow}
	candidate, err := readCacheCandidate(context.Background(), db, entry)
	if err != nil {
		t.Fatalf("read cache candidate: %v", err)
	}
	if err := candidate.Validate(); err != nil {
		t.Fatalf("durable order candidate invalid: %v", err)
	}
	if got := []domain.ArtifactOccurrenceID{candidate.Entry.Sources[0].OccurrenceID, candidate.Entry.Sources[1].OccurrenceID}; got[0] != firstOccurrence || got[1] != secondOccurrence {
		t.Fatalf("source order = %v, want [%s %s]", got, firstOccurrence, secondOccurrence)
	}
}

func TestCommitReuseScalarRejectsCollectionBeforeWritingRows(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE cache_entries (cache_key_digest TEXT, kind TEXT, schema_version TEXT, policy_digest TEXT, input_digest TEXT, state TEXT, expires_at TEXT, created_at TEXT)`,
		`CREATE TABLE cache_entry_sources (cache_key_digest TEXT, source_run_id TEXT, source_call_record_id TEXT, source_occurrence_id TEXT, source_digest TEXT)`,
		`CREATE TABLE cache_blob_refs (cache_key_digest TEXT, digest TEXT, size INTEGER, role TEXT, media_type TEXT, logical_path TEXT, provenance_json BLOB, provenance_digest TEXT)`,
		`CREATE TABLE cache_blob_order (cache_key_digest TEXT, blob_ordinal INTEGER, digest TEXT, size INTEGER, role TEXT, source_occurrence_id TEXT)`,
		`CREATE TABLE call_records (call_record_id TEXT, run_id TEXT, stage_name TEXT, attempt_id TEXT, dispatch_kind TEXT)`,
		`CREATE TABLE cache_reuse_records (cache_reuse_record_id TEXT, run_id TEXT, stage_name TEXT, attempt_id TEXT, current_call_record_id TEXT, source_call_record_id TEXT, source_occurrence_id TEXT, digest TEXT, size INTEGER, cache_key_digest TEXT, created_at TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db, config: Config{BusyTimeout: time.Second, MaxReaders: 1}, clock: clock.NewFake(testNow)}
	key := domain.CacheKey{Digest: domain.SumBytes([]byte("scalar-collection-key")), Kind: "statement"}
	run := domain.RunID("run_00000000000000000000000000000022")
	stage := domain.StageName("prepare")
	attempt := domain.AttemptID("attempt_00000000000000000000000000000022")
	sourceCall := domain.CallRecordID("callrec_00000000000000000000000000000022")
	currentCall := domain.CallRecordID("callrec_00000000000000000000000000000023")
	sourceIDs := []domain.ArtifactOccurrenceID{
		"occurrence_ffffffffffffffffffffffffffffffff",
		"occurrence_00000000000000000000000000000000",
	}
	data := [][]byte{[]byte("first"), []byte("second")}
	roles := []domain.ArtifactRole{domain.ArtifactOutput, domain.ArtifactStdout}
	provenance := domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "scalar-test"}
	payload, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cache_entries VALUES (?, ?, ?, ?, ?, 'VALID', NULL, ?)`, key.Digest, key.Kind, "cpgen.cache/v1", domain.SumBytes([]byte("policy")), domain.SumBytes([]byte("input")), formatTime(testNow)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO call_records VALUES (?, ?, ?, ?, ?)`, sourceCall, run, stage, attempt, domain.DispatchDispatched); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO call_records VALUES (?, ?, ?, ?, ?)`, currentCall, run, stage, attempt, domain.DispatchCacheHit); err != nil {
		t.Fatal(err)
	}
	for index, sourceID := range sourceIDs {
		digest := domain.SumBytes(data[index])
		if _, err := db.Exec(`INSERT INTO cache_entry_sources VALUES (?, ?, ?, ?, ?)`, key.Digest, run, sourceCall, sourceID, digest); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO cache_blob_refs VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, key.Digest, digest, len(data[index]), roles[index], "text/plain", string(rune('a'+index)), payload, domain.SumBytes(payload)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO cache_blob_order VALUES (?, ?, ?, ?, ?, ?)`, key.Digest, index+1, digest, len(data[index]), roles[index], sourceID); err != nil {
			t.Fatal(err)
		}
	}
	request := domain.CommitCacheReuse{
		RunID: run, StageName: stage, AttemptID: attempt, CurrentCallRecordID: currentCall,
		CacheReuseRecordIDs: []domain.CacheReuseRecordID{
			"reuse_ffffffffffffffffffffffffffffffff",
			"reuse_00000000000000000000000000000000",
		},
		Key: key, SourceCallRecordID: sourceCall, SourceOccurrenceIDs: sourceIDs, At: testNow,
	}
	if _, err := store.CommitReuse(context.Background(), request); err == nil {
		t.Fatal("scalar CommitReuse accepted a multi-Blob collection")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM cache_reuse_records`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("scalar rejection left %d reuse rows", count)
	}
}

func TestMigrationThirteenBackfillsSameDigestBlobsByRole(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache-order.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	migrations, err := loadMigrations()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if len(migrations) < 13 {
		_ = db.Close()
		t.Fatalf("loaded %d migrations, want M13", len(migrations))
	}
	for _, migration := range migrations[:12] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			_ = db.Close()
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatalf("record migration %d: %v", migration.version, err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, trigger := range []string{
		"artifact_occurrences_ready_insert", "artifact_occurrence_new_write_scope_insert", "artifact_occurrence_cache_scope_insert", "artifact_occurrence_new_write_reservation_scope_insert",
		"cache_entry_sources_insert_guard", "cache_blob_refs_insert_guard", "cache_blob_order_insert_guard",
	} {
		if _, err := db.ExecContext(ctx, "DROP TRIGGER "+trigger); err != nil {
			_ = db.Close()
			t.Fatalf("drop fixture trigger %s: %v", trigger, err)
		}
	}
	// Seed the state that M12's digest-only backfill could not distinguish:
	// two roles share one immutable Blob, while each role has a different
	// producing occurrence.
	key := domain.CacheKey{Digest: domain.SumBytes([]byte("same-digest-cache-key")), Kind: "statement"}
	sharedData := []byte("same digest")
	sharedDigest := domain.SumBytes(sharedData)
	run := domain.RunID("run_00000000000000000000000000000024")
	stage := domain.StageName("prepare")
	attempt := domain.AttemptID("attempt_00000000000000000000000000000024")
	call := domain.CallRecordID("callrec_00000000000000000000000000000024")
	occOutput := domain.ArtifactOccurrenceID("occurrence_ffffffffffffffffffffffffffffffff")
	occStdout := domain.ArtifactOccurrenceID("occurrence_00000000000000000000000000000000")
	provenance := domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "migration-test"}
	payload, err := json.Marshal(provenance)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO blobs(digest, size, state, canonical_relative_path, verified_at, gc_state) VALUES (?, ?, 'READY', ?, ?, 'NONE')`, sharedDigest, len(sharedData), "blobs/sha256/shared", formatTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO cache_entries(cache_key_digest, kind, schema_version, policy_digest, input_digest, state, expires_at, invalidation_cause, created_at) VALUES (?, ?, ?, ?, ?, 'VALID', NULL, NULL, ?)`, key.Digest, key.Kind, "cpgen.cache/v1", domain.SumBytes([]byte("policy")), domain.SumBytes([]byte("input")), formatTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	// The source occurrence insert guards are intentionally bypassed here: the
	// fixture models an already durable historical database and only needs the
	// role-bearing rows that M13 repairs.
	for _, item := range []struct {
		id   domain.ArtifactOccurrenceID
		role domain.ArtifactRole
		path string
	}{
		{occOutput, domain.ArtifactOutput, "output"},
		{occStdout, domain.ArtifactStdout, "stdout"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO artifact_occurrences(occurrence_id, kind, run_id, stage_name, attempt_id, current_call_record_id, source_occurrence_id, cache_reuse_record_id, source_call_record_id, digest, size, role, logical_path, media_type, provenance_json, provenance_digest, created_at) VALUES (?, 'CACHE_REUSE', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'text/plain', ?, ?, ?)`, item.id, run, stage, attempt, call, item.id, "reuse_"+string(item.id[len("occurrence_"):]), call, sharedDigest, len(sharedData), item.role, item.path, payload, domain.SumBytes(payload), formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO cache_entry_sources(cache_key_digest, source_run_id, source_call_record_id, source_occurrence_id, source_digest) VALUES (?, ?, ?, ?, ?)`, key.Digest, run, call, item.id, sharedDigest); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO cache_blob_refs(cache_key_digest, digest, size, role, media_type, logical_path, provenance_json, provenance_digest) VALUES (?, ?, ?, ?, 'text/plain', ?, ?, ?)`, key.Digest, sharedDigest, len(sharedData), item.role, item.path, payload, domain.SumBytes(payload)); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	// Reproduce M12's bad same-digest choice: both ordered rows point at the
	// first occurrence, even though their roles identify different sources.
	for ordinal, role := range []domain.ArtifactRole{domain.ArtifactOutput, domain.ArtifactStdout} {
		if _, err := db.ExecContext(ctx, `INSERT INTO cache_blob_order(cache_key_digest, blob_ordinal, digest, size, role, source_occurrence_id) VALUES (?, ?, ?, ?, ?, ?)`, key.Digest, ordinal+1, sharedDigest, len(sharedData), role, occOutput); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, migrations[12].sql); err != nil {
		_ = db.Close()
		t.Fatalf("apply M13 repair: %v", err)
	}
	var gotOutput, gotStdout string
	if err := db.QueryRowContext(ctx, `SELECT source_occurrence_id FROM cache_blob_order WHERE cache_key_digest = ? AND role = ?`, key.Digest, domain.ArtifactOutput).Scan(&gotOutput); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT source_occurrence_id FROM cache_blob_order WHERE cache_key_digest = ? AND role = ?`, key.Digest, domain.ArtifactStdout).Scan(&gotStdout); err != nil {
		t.Fatal(err)
	}
	if gotOutput != string(occOutput) || gotStdout != string(occStdout) {
		t.Fatalf("same-digest backfill = output:%q stdout:%q, want output:%q stdout:%q", gotOutput, gotStdout, occOutput, occStdout)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
