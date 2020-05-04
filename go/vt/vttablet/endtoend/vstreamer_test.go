/*
Copyright 2020 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endtoend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/log"
	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	querypb "vitess.io/vitess/go/vt/proto/query"
	tabletpb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/srvtopo"
	"vitess.io/vitess/go/vt/vttablet/endtoend/framework"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/vstreamer"
)

type test struct {
	query  string
	output []string
}

func TestSchemaVersioning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	tsv := framework.Server
	origWatchReplication := tsv.Config().WatchReplication
	tsv.Config().WatchReplication = true
	defer func() {
		tsv.Config().WatchReplication = origWatchReplication

	}()
	tsv.Historian().SetTrackSchemaVersions(true)
	tsv.StartTracker()
	srvTopo := srvtopo.NewResilientServer(framework.TopoServer, "SchemaVersionE2ETestTopo")

	vstreamer.NewEngine(tabletenv.NewEnv(tsv.Config(), "SchemaVersionE2ETest"), srvTopo, tsv.Historian())
	target := &querypb.Target{
		Keyspace:   "vttest",
		Shard:      "0",
		TabletType: tabletpb.TabletType_MASTER,
		Cell:       "",
	}
	filter := &binlogdatapb.Filter{
		Rules: []*binlogdatapb.Rule{{
			Match: "/.*/",
		}},
	}

	var cases = []test{
		{
			query: "create table vitess_version (id1 int, id2 int)",
			output: []string{
				`gtid`,
				`other`,
				`gtid`,
				`type:DDL ddl:"create table vitess_version (id1 int, id2 int)" `,
				`gtid`,
				`other`,
				`version`,
				`gtid`,
			},
		},
		{
			query: "insert into vitess_version values(1, 10)",
			output: []string{
				`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > > `,
				`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 values:"110" > > > `,
				`gtid`,
			},
		}, {
			query: "alter table vitess_version add column id3 int",
			output: []string{
				`gtid`,
				`type:DDL ddl:"alter table vitess_version add column id3 int" `,
				`gtid`,
				`other`,
				`version`,
				`gtid`,
			},
		}, {
			query: "insert into vitess_version values(2, 20, 200)",
			output: []string{
				`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > fields:<name:"id3" type:INT32 > > `,
				`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 values:"220200" > > > `,
				`gtid`,
			},
		}, {
			query: "alter table vitess_version modify column id3 varbinary(16)",
			output: []string{
				`gtid`,
				`type:DDL ddl:"alter table vitess_version modify column id3 varbinary(16)" `,
				`gtid`,
				`other`,
				`version`,
				`gtid`,
			},
		}, {
			query: "insert into vitess_version values(3, 30, 'TTT')",
			output: []string{
				`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > fields:<name:"id3" type:VARBINARY > > `,
				`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 values:"330TTT" > > > `,
				`gtid`,
			},
		},
	}
	eventCh := make(chan []*binlogdatapb.VEvent)
	var startPos string
	send := func(events []*binlogdatapb.VEvent) error {
		var evs []*binlogdatapb.VEvent
		for _, event := range events {
			if event.Type == binlogdatapb.VEventType_GTID {
				if startPos == "" {
					startPos = event.Gtid
				}
			}
			if event.Type == binlogdatapb.VEventType_HEARTBEAT {
				continue
			}
			log.Infof("Received event %v", event)
			evs = append(evs, event)
		}
		select {
		case eventCh <- evs:
		case <-ctx.Done():
			t.Fatal("Context Done() in send")
		}
		return nil
	}
	go func() {
		defer close(eventCh)
		if err := tsv.VStream(ctx, target, "current", filter, send); err != nil {
			fmt.Printf("Error in tsv.VStream: %v", err)
			t.Error(err)
		}
	}()
	log.Infof("\n\n\n=============================================== CURRENT EVENTS START HERE ======================\n\n\n")
	runCases(ctx, t, cases, eventCh)

	tsv.StopTracker()
	cases = []test{
		{
			//comment prefix required so we don't look for ddl in schema_version
			query: "/**/alter table vitess_version add column id4 varbinary(16)",
			output: []string{
				`gtid`,
				`type:DDL ddl:"alter table vitess_version add column id4 varbinary(16)" `,
			},
		}, {
			query: "insert into vitess_version values(4, 40, 'FFF', 'GGGG' )",
			output: []string{
				`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > fields:<name:"id3" type:VARBINARY > fields:<name:"id4" type:VARBINARY > > `,
				`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 lengths:4 values:"440FFFGGGG" > > > `,
				`gtid`,
			},
		},
	}
	runCases(ctx, t, cases, eventCh)
	cancel()
	log.Infof("\n\n\n=============================================== PAST EVENTS START HERE ======================\n\n\n")
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	eventCh = make(chan []*binlogdatapb.VEvent)
	send = func(events []*binlogdatapb.VEvent) error {
		var evs []*binlogdatapb.VEvent
		for _, event := range events {
			if event.Type == binlogdatapb.VEventType_HEARTBEAT {
				continue
			}
			log.Infof("Received event %v", event)
			evs = append(evs, event)
		}
		select {
		case eventCh <- evs:
		case <-ctx.Done():
			t.Fatal("Context Done() in send")
		}
		return nil
	}
	go func() {
		defer close(eventCh)
		if err := tsv.VStream(ctx, target, startPos, filter, send); err != nil {
			fmt.Printf("Error in tsv.VStream: %v", err)
			t.Error(err)
		}
	}()

	output := []string{
		`gtid`,
		`type:DDL ddl:"create table vitess_version (id1 int, id2 int)" `,
		`gtid`,
		`other`,
		`version`,
		`gtid`,
		`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > > `,
		`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 values:"110" > > > `,
		`gtid`,
		`gtid`,
		`type:DDL ddl:"alter table vitess_version add column id3 int" `,
		`gtid`,
		`other`,
		`version`,
		`gtid`,
		`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > fields:<name:"id3" type:INT32 > > `,
		`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 values:"220200" > > > `,
		`gtid`,
		`gtid`,
		`type:DDL ddl:"alter table vitess_version modify column id3 varbinary(16)" `,
		`gtid`,
		`other`,
		`version`,
		`gtid`,
		`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > fields:<name:"id3" type:VARBINARY > > `,
		`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 values:"330TTT" > > > `,
		`gtid`,
		`gtid`,
		`type:DDL ddl:"alter table vitess_version add column id4 varbinary(16)" `,
		`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > fields:<name:"id3" type:VARBINARY > fields:<name:"id4" type:VARBINARY > > `,
		`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 lengths:4 values:"440FFFGGGG" > > > `,
		`gtid`,
	}

	expectLogs(ctx, t, "Past stream", eventCh, output)

	cancel()

	log.Infof("\n\n\n=============================================== PAST EVENTS WITHOUT TRACK VERSIONS START HERE ======================\n\n\n")
	tsv.Historian().SetTrackSchemaVersions(false)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	eventCh = make(chan []*binlogdatapb.VEvent)
	send = func(events []*binlogdatapb.VEvent) error {
		var evs []*binlogdatapb.VEvent
		for _, event := range events {
			if event.Type == binlogdatapb.VEventType_HEARTBEAT {
				continue
			}
			log.Infof("Received event %v", event)
			evs = append(evs, event)
		}
		select {
		case eventCh <- evs:
		case <-ctx.Done():
			t.Fatal("Context Done() in send")
		}
		return nil
	}
	go func() {
		defer close(eventCh)
		if err := tsv.VStream(ctx, target, startPos, filter, send); err != nil {
			fmt.Printf("Error in tsv.VStream: %v", err)
			t.Error(err)
		}
	}()

	output = []string{
		`gtid`,
		`type:DDL ddl:"create table vitess_version (id1 int, id2 int)" `,
		`gtid`,
		`other`,
		`version`,
		`gtid`,
		`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > > `,
		`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 values:"110" > > > `,
		`gtid`,
		`gtid`,
		`type:DDL ddl:"alter table vitess_version add column id3 int" `,
		`gtid`,
		`other`,
		`version`,
		`gtid`,
		`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"@1" type:INT32 > fields:<name:"@2" type:INT32 > fields:<name:"@3" type:INT32 > > `,
		`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 values:"220200" > > > `,
		`gtid`,
		`gtid`,
		`type:DDL ddl:"alter table vitess_version modify column id3 varbinary(16)" `,
		`gtid`,
		`other`,
		`version`,
		`gtid`,
		`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > fields:<name:"id3" type:VARBINARY > > `,
		`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 values:"330TTT" > > > `,
		`gtid`,
		`gtid`,
		`type:DDL ddl:"alter table vitess_version add column id4 varbinary(16)" `,
		`type:FIELD field_event:<table_name:"vitess_version" fields:<name:"id1" type:INT32 > fields:<name:"id2" type:INT32 > fields:<name:"id3" type:VARBINARY > fields:<name:"id4" type:VARBINARY > > `,
		`type:ROW row_event:<table_name:"vitess_version" row_changes:<after:<lengths:1 lengths:2 lengths:3 lengths:4 values:"440FFFGGGG" > > > `,
		`gtid`,
	}

	expectLogs(ctx, t, "Past stream", eventCh, output)
	cancel()

	client := framework.NewClient()
	client.Execute("drop table vitess_version", nil)
	client.Execute("drop table _vt.schema_version", nil)

	log.Info("=== END OF TEST")
}

func runCases(ctx context.Context, t *testing.T, tests []test, eventCh chan []*binlogdatapb.VEvent) {
	client := framework.NewClient()

	for _, test := range tests {
		query := test.query
		client.Execute(query, nil)
		if len(test.output) > 0 {
			expectLogs(ctx, t, query, eventCh, test.output)
		}
		if strings.HasPrefix(query, "create") || strings.HasPrefix(query, "alter") || strings.HasPrefix(query, "drop") {
			ok, err := waitForVersionInsert(client, query)
			if err != nil || !ok {
				t.Fatalf("Query %s never got inserted into the schema_version table", query)
			}
		}
	}
}

func expectLogs(ctx context.Context, t *testing.T, query string, eventCh chan []*binlogdatapb.VEvent, output []string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var evs []*binlogdatapb.VEvent
	log.Infof("In expectLogs for query %s, output len %s", query, len(output))
	for {
		select {
		case allevs, ok := <-eventCh:
			if !ok {
				t.Fatal("expectLogs: not ok, stream ended early")
			}
			for _, ev := range allevs {
				// Ignore spurious heartbeats that can happen on slow machines.
				if ev.Type == binlogdatapb.VEventType_HEARTBEAT {
					continue
				}
				if ev.Type == binlogdatapb.VEventType_BEGIN {
					continue
				}
				if ev.Type == binlogdatapb.VEventType_COMMIT {
					continue
				}

				evs = append(evs, ev)
			}
			log.Infof("In expectLogs, have got %d events, want %d", len(evs), len(output))
		case <-ctx.Done():
			t.Fatalf("expectLog: Done(), stream ended early")
		case <-timer.C:
			t.Fatalf("expectLog: timed out waiting for events: %v: evs\n%v, want\n%v, >> got length %d, wanted length %d", query, evs, output, len(evs), len(output))
		}
		if len(evs) >= len(output) {
			break
		}
	}
	if len(evs) > len(output) {
		t.Fatalf("expectLog: got too many events: %v: evs\n%v, want\n%v, >> got length %d, wanted length %d", query, evs, output, len(evs), len(output))
	}
	for i, want := range output {
		// CurrentTime is not testable.
		evs[i].CurrentTime = 0
		switch want {
		case "begin":
			if evs[i].Type != binlogdatapb.VEventType_BEGIN {
				t.Fatalf("%v (%d): event: %v, want begin", query, i, evs[i])
			}
		case "gtid":
			if evs[i].Type != binlogdatapb.VEventType_GTID {
				t.Fatalf("%v (%d): event: %v, want gtid", query, i, evs[i])
			}
		case "commit":
			if evs[i].Type != binlogdatapb.VEventType_COMMIT {
				t.Fatalf("%v (%d): event: %v, want commit", query, i, evs[i])
			}
		case "other":
			if evs[i].Type != binlogdatapb.VEventType_OTHER {
				t.Fatalf("%v (%d): event: %v, want other", query, i, evs[i])
			}
		case "version":
			if evs[i].Type != binlogdatapb.VEventType_VERSION {
				t.Fatalf("%v (%d): event: %v, want version", query, i, evs[i])
			}
		default:
			evs[i].Timestamp = 0
			if got := fmt.Sprintf("%v", evs[i]); got != want {
				t.Fatalf("%v (%d): event:\n%q, want\n%q", query, i, got, want)
			}
		}
	}
}

func encodeString(in string) string {
	buf := bytes.NewBuffer(nil)
	sqltypes.NewVarChar(in).EncodeSQL(buf)
	return buf.String()
}

func validateSchemaInserted(client *framework.QueryClient, ddl string) (bool, error) {
	qr, _ := client.Execute(fmt.Sprintf("select * from _vt.schema_version where ddl = %s", encodeString(ddl)), nil)
	if len(qr.Rows) == 1 {
		log.Infof("Found ddl in schema_version: %s", ddl)
		return true, nil
	}
	return false, fmt.Errorf("Found %d rows for gtid %s", len(qr.Rows), ddl)
}

func waitForVersionInsert(client *framework.QueryClient, ddl string) (bool, error) {
	timeout := time.After(1000 * time.Millisecond)
	tick := time.Tick(100 * time.Millisecond)
	for {
		select {
		case <-timeout:
			return false, errors.New("waitForVersionInsert timed out")
		case <-tick:
			ok, err := validateSchemaInserted(client, ddl)
			if err != nil {
				return false, err
			} else if ok {
				log.Infof("Found version insert for %s", ddl)
				return true, nil
			}
		}
	}
}
