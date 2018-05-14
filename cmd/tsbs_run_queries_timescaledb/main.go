// tsbs_run_queries_timescaledb speed tests TimescaleDB using requests from stdin.
//
// It reads encoded Query objects from stdin, and makes concurrent requests
// to the provided PostgreSQL/TimescaleDB endpoint. This program has no knowledge of the
// internals of the endpoint.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"regexp"
	"strings"
	"time"

	"bitbucket.org/440-labs/influxdb-comparisons/query"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// Program option vars:
var (
	postgresConnect string
	databaseName    string
	hostList        []string
	user            string
	showExplain     bool
)

// Global vars:
var (
	benchmarkRunner *query.BenchmarkRunner
)

// Parse args:
func init() {
	benchmarkRunner = query.NewBenchmarkRunner()
	var hosts string

	flag.StringVar(&postgresConnect, "postgres", "host=postgres user=postgres sslmode=disable",
		"String of additional PostgreSQL connection parameters, e.g., 'sslmode=disable'. Parameters for host and database will be ignored.")
	flag.StringVar(&databaseName, "db-name", "benchmark", "Name of database to use for queries")
	flag.StringVar(&hosts, "hosts", "localhost", "Comma separated list of PostgreSQL hosts (pass multiple values for sharding reads on a multi-node setup)")
	flag.StringVar(&user, "user", "postgres", "User to connect to PostgreSQL as")

	flag.BoolVar(&showExplain, "show-explain", false, "Print out the EXPLAIN output for sample query")

	flag.Parse()

	if showExplain {
		benchmarkRunner.ResetLimit(1)
	}

	// Parse comma separated string of hosts and put in a slice (for multi-node setups)
	for _, host := range strings.Split(hosts, ",") {
		hostList = append(hostList, host)
	}
}

func main() {
	benchmarkRunner.Run(&query.TimescaleDBPool, newProcessor)
}

// Get the connection string for a connection to PostgreSQL.

// If we're running queries against multiple nodes we need to balance the queries
// across replicas. Each worker is assigned a sequence number -- we'll use that
// to evenly distribute hosts to worker connections
func getConnectString(workerNumber int) string {
	// User might be passing in host=hostname the connect string out of habit which may override the
	// multi host configuration. Same for dbname= and user=. This sanitizes that.
	re := regexp.MustCompile(`(host|dbname|user)=\S*\b`)
	connectString := re.ReplaceAllString(postgresConnect, "")

	// Round robin the host/worker assignment by assigning a host based on workerNumber % totalNumberOfHosts
	host := hostList[workerNumber%len(hostList)]
	return fmt.Sprintf("host=%s dbname=%s user=%s %s", host, databaseName, user, connectString)
}

// prettyPrintResponse prints a Query and its response in JSON format with two
// keys: 'query' which has a value of the SQL used to generate the second key
// 'results' which is an array of each row in the return set.
func prettyPrintResponse(rows *sqlx.Rows, q *query.TimescaleDB) {
	resp := make(map[string]interface{})
	resp["query"] = string(q.SqlQuery)

	results := []map[string]interface{}{}
	for rows.Next() {
		r := make(map[string]interface{})
		if err := rows.MapScan(r); err != nil {
			panic(err)
		}
		results = append(results, r)
		resp["results"] = results
	}

	line, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		panic(err)
	}

	fmt.Println(string(line) + "\n")
}

type queryExecutorOptions struct {
	showExplain   bool
	debug         bool
	printResponse bool
}

type queryExecutor struct {
	db *sqlx.DB
}

func newQueryExecutor(conn string) *queryExecutor {
	return &queryExecutor{
		db: sqlx.MustConnect("postgres", conn),
	}
}

func (qe *queryExecutor) Do(q query.Query, opts *queryExecutorOptions) (float64, error) {
	start := time.Now()
	qry := string(q.(*query.TimescaleDB).SqlQuery)
	if showExplain {
		qry = "EXPLAIN ANALYZE " + qry
	}
	rows, err := qe.db.Queryx(qry)
	took := float64(time.Since(start).Nanoseconds()) / 1e6
	if err != nil {
		return took, err
	}

	if opts.debug {
		fmt.Println(qry)
	}
	if showExplain {
		text := ""
		for rows.Next() {
			var s string
			if err2 := rows.Scan(&s); err2 != nil {
				panic(err2)
			}
			text += s + "\n"
		}
		fmt.Printf("%s\n\n%s\n-----\n\n", qry, text)
	} else if opts.printResponse {
		prettyPrintResponse(rows, q.(*query.TimescaleDB))
	}
	rows.Close()
	took = float64(time.Since(start).Nanoseconds()) / 1e6
	return took, err
}

type processor struct {
	qe   *queryExecutor
	opts *queryExecutorOptions
}

func newProcessor() query.Processor { return &processor{} }

func (p *processor) Init(workerNumber int) {
	p.qe = newQueryExecutor(getConnectString(workerNumber))
	p.opts = &queryExecutorOptions{
		showExplain:   showExplain,
		debug:         benchmarkRunner.DebugLevel() > 0,
		printResponse: benchmarkRunner.DoPrintResponses(),
	}
}

// processQueries reads byte buffers from queryChan and writes them to the
// target server, while tracking latency.
func (p *processor) ProcessQuery(sp *query.StatProcessor, q query.Query) {
	lag, err := p.qe.Do(q, p.opts)
	if err != nil {
		panic(err)
	}
	sp.SendStat(q.HumanLabelName(), lag, !sp.PrewarmQueries)

	// If PrewarmQueries is set, we run the query as 'cold' first (see above),
	// then we immediately run it a second time and report that as the 'warm'
	// stat. This guarantees that the warm stat will reflect optimal cache performance.
	if !showExplain && sp.PrewarmQueries {
		// Warm run
		lag, err = p.qe.Do(q, &queryExecutorOptions{})
		if err != nil {
			panic(err)
		}
		sp.SendStat(q.HumanLabelName(), lag, true)
	}
}
