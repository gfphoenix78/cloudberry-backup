package end_to_end_test

import (
	"fmt"

	"github.com/apache/cloudberry-go-libs/dbconn"
	"github.com/apache/cloudberry-go-libs/testhelper"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

/*
 * A materialized view holds data the way a table does -- its own storage, its
 * own access method -- but gpbackup treats it as metadata only. The data set
 * is restricted to relkind 'r' (and 'p' for partition roots), so a
 * materialized view is never read, never appears in a restore plan, and is
 * emitted as CREATE MATERIALIZED VIEW ... WITH NO DATA. gprestore issues no
 * REFRESH, so what comes back is defined but unpopulated.
 *
 * That is worth pinning down rather than assuming: it is the difference
 * between "the restore lost my data" and "the restore did what it documents",
 * and it is the answer to why refreshing a materialized view never shows up in
 * the next incremental backup.
 */

const matviewSchema = "matview"

func createMatviewSchema(conn *dbconn.DBConn, accessMethods []string) {
	testhelper.AssertQueryRuns(conn, fmt.Sprintf(
		"DROP SCHEMA IF EXISTS %s CASCADE; CREATE SCHEMA %s", matviewSchema, matviewSchema))
	testhelper.AssertQueryRuns(conn, fmt.Sprintf(
		"CREATE TABLE %s.base (id int, part int, v text) DISTRIBUTED BY (id)", matviewSchema))
	testhelper.AssertQueryRuns(conn, fmt.Sprintf(
		"INSERT INTO %s.base SELECT i, i %% 3, 'r' || i FROM generate_series(1, 300) i", matviewSchema))

	for _, am := range accessMethods {
		testhelper.AssertQueryRuns(conn, fmt.Sprintf(
			`CREATE MATERIALIZED VIEW %s.mv_%s USING %s AS
				SELECT id, part, v FROM %s.base DISTRIBUTED BY (id)`,
			matviewSchema, am, am, matviewSchema))
	}

	/*
	 * One more view carrying a storage option whose value holds a comma, so
	 * the round trip covers option quoting on the materialized view path.
	 *
	 * It is kept apart from the views above because it cannot be refreshed
	 * once restored. PAX validates a column-list option against the wrong
	 * tuple descriptor when refreshing a view that was created WITH NO DATA,
	 * and since that is exactly how gpbackup emits every materialized view,
	 * a restored one fails with
	 *
	 *   ERROR: invalid column name 'id' (paxc_rel_options.cc:382)
	 *
	 * That reproduces with no gpbackup involved: create the same view WITH
	 * NO DATA by hand and refresh it. Created WITH DATA, or created WITH NO
	 * DATA but without the option, the refresh succeeds.
	 */
	if paxIsAvailable(accessMethods) {
		testhelper.AssertQueryRuns(conn, fmt.Sprintf(
			`CREATE MATERIALIZED VIEW %s.%s USING pax WITH (minmax_columns='id,part') AS
				SELECT id, part, v FROM %s.base DISTRIBUTED BY (id)`,
			matviewSchema, matviewWithOptions, matviewSchema))
	}
}

const matviewWithOptions = "mv_pax_options"

func paxIsAvailable(accessMethods []string) bool {
	for _, am := range accessMethods {
		if am == "pax" {
			return true
		}
	}
	return false
}

// The views that can be refreshed after a restore.
func matviewNames(accessMethods []string) []string {
	names := make([]string, 0, len(accessMethods))
	for _, am := range accessMethods {
		names = append(names, fmt.Sprintf("%s.mv_%s", matviewSchema, am))
	}
	return names
}

// Every view in the schema, including the one that cannot be refreshed.
func allMatviewNames(accessMethods []string) []string {
	names := matviewNames(accessMethods)
	if paxIsAvailable(accessMethods) {
		names = append(names, fmt.Sprintf("%s.%s", matviewSchema, matviewWithOptions))
	}
	return names
}

type matviewState struct {
	AccessMethod   string
	StorageOptions string
	Populated      bool
}

// How each materialized view in the schema is declared, and whether it holds
// anything.
func getMatviewState(conn *dbconn.DBConn) map[string]matviewState {
	var results []struct {
		RelName        string
		AccessMethod   string
		StorageOptions string
		Populated      bool
	}
	err := conn.Select(&results, fmt.Sprintf(`
		SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) AS relname,
			coalesce(a.amname, '') AS accessmethod,
			coalesce(pg_catalog.array_to_string(c.reloptions, ', '), '') AS storageoptions,
			c.relispopulated AS populated
		FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			LEFT JOIN pg_am a ON a.oid = c.relam
		WHERE n.nspname = '%s'
			AND c.relkind = 'm'`, matviewSchema))
	Expect(err).ToNot(HaveOccurred())

	state := make(map[string]matviewState, len(results))
	for _, result := range results {
		state[result.RelName] = matviewState{
			AccessMethod:   result.AccessMethod,
			StorageOptions: result.StorageOptions,
			Populated:      result.Populated,
		}
	}
	return state
}

var _ = Describe("End to End materialized view tests", func() {
	var accessMethods []string
	var matviews []string
	var allMatviews []string

	BeforeEach(func() {
		end_to_end_setup()
		if backupConn.Version.IsGPDB() && backupConn.Version.Before("7") {
			Skip("Test uses USING <access method>, which needs GPDB7 or Cloudberry")
		}
		skipIfOldBackupVersionBefore("2.2.0")

		accessMethods = matrixAccessMethods()
		matviews = matviewNames(accessMethods)
		allMatviews = allMatviewNames(accessMethods)
		createMatviewSchema(backupConn, accessMethods)
	})
	AfterEach(func() {
		testhelper.AssertQueryRuns(backupConn,
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", matviewSchema))
		testhelper.AssertQueryRuns(restoreConn,
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", matviewSchema))
		end_to_end_teardown()
	})

	Describe("Full backup and restore", func() {
		It("restores every materialized view under its original access method and storage options", func() {
			output := gpbackup(gpbackupPath, backupHelperPath,
				"--leaf-partition-data", "--include-schema", matviewSchema)
			timestamp := getBackupTimestamp(string(output))
			defer assertArtifactsCleaned(timestamp)

			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

			backedUp := getMatviewState(backupConn)
			restored := getMatviewState(restoreConn)
			Expect(restored).To(HaveLen(len(allMatviews)))

			for _, matview := range allMatviews {
				// The source views are populated and the restored ones are
				// not, so compare only how they are declared.
				Expect(restored[matview].AccessMethod).To(Equal(backedUp[matview].AccessMethod),
					"%s should come back under its original access method", matview)
				Expect(restored[matview].StorageOptions).To(Equal(backedUp[matview].StorageOptions),
					"%s should come back with its original storage options", matview)
			}
		})

		It("leaves materialized views unpopulated, and a refresh reproduces the source", func() {
			output := gpbackup(gpbackupPath, backupHelperPath,
				"--leaf-partition-data", "--include-schema", matviewSchema)
			timestamp := getBackupTimestamp(string(output))
			defer assertArtifactsCleaned(timestamp)

			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

			/*
			 * gpbackup emits WITH NO DATA and never issues a REFRESH, so the
			 * views arrive defined but empty. Reading one raises "materialized
			 * view ... has not been populated", which is why this checks
			 * relispopulated rather than counting rows.
			 */
			restored := getMatviewState(restoreConn)
			for _, matview := range allMatviews {
				Expect(restored[matview].Populated).To(BeFalse(),
					"%s should be restored unpopulated", matview)
			}

			// The base table's data does come back, so a refresh is enough to
			// reproduce what the source held.
			assertDataRestored(restoreConn, map[string]int{matviewSchema + ".base": 300})
			for _, matview := range matviews {
				testhelper.AssertQueryRuns(restoreConn,
					fmt.Sprintf("REFRESH MATERIALIZED VIEW %s", matview))
			}
			Expect(getMatrixContents(restoreConn, matviews)).
				To(Equal(getMatrixContents(backupConn, matviews)))
		})
	})

	Describe("Incremental backup", func() {
		It("never reads a materialized view, however the base table or the view changes", func() {
			output := gpbackup(gpbackupPath, backupHelperPath,
				"--leaf-partition-data", "--include-schema", matviewSchema)
			fullTimestamp := getBackupTimestamp(string(output))
			defer assertArtifactsCleaned(fullTimestamp)

			testhelper.AssertQueryRuns(backupConn, fmt.Sprintf(
				"INSERT INTO %s.base SELECT i, i %% 3, 'new' FROM generate_series(1001, 1010) i", matviewSchema))
			for _, matview := range matviews {
				testhelper.AssertQueryRuns(backupConn,
					fmt.Sprintf("REFRESH MATERIALIZED VIEW %s", matview))
			}

			output = gpbackup(gpbackupPath, backupHelperPath,
				"--leaf-partition-data", "--incremental", "--include-schema", matviewSchema)
			incrementalTimestamp := getBackupTimestamp(string(output))
			defer assertArtifactsCleaned(incrementalTimestamp)

			/*
			 * The base table is heap, so it is re-read unconditionally. The
			 * materialized views are absent from both entries: refreshing one
			 * rewrites its storage, but gpbackup never had its data to begin
			 * with, so there is nothing to bring forward.
			 */
			plan := getRestorePlan(incrementalTimestamp)
			Expect(plan[incrementalTimestamp]).To(ConsistOf(matviewSchema + ".base"))
			for _, entry := range plan {
				for _, matview := range allMatviews {
					Expect(entry).To(Not(ContainElement(matview)),
						"%s should never appear in a restore plan", matview)
				}
			}
		})
	})
})
