package main

import (
	"database/sql"
	"testing"
)

func TestLoadPortMapBindsPortsToRequestedHostAndIgnoresStoredIP(t *testing.T) {
	db := newTestDB(t)

	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 1, "10.0.0.8", 4001, 300)
	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 2, "10.0.0.195", 4001, 2000)
	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 3, "10.0.0.8", 0, 60000)
	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 4, "10.0.0.8", 70000, 60000)
	mustExec(t, db, "INSERT INTO commands (port_id, command_key, return_value, seq) VALUES (?, ?, ?, ?)", 1, "AA", "BB", 0)
	mustExec(t, db, "INSERT INTO commands (port_id, command_key, return_value, seq) VALUES (?, ?, ?, ?)", 2, "CC", "DD", 0)
	mustExec(t, db, "INSERT INTO commands (port_id, command_key, return_value, seq) VALUES (?, ?, ?, ?)", 3, "EE", "FF", 0)
	mustExec(t, db, "INSERT INTO commands (port_id, command_key, return_value, seq) VALUES (?, ?, ?, ?)", 4, "11", "22", 0)

	pm, err := LoadPortMap(db, "127.0.0.1")
	if err != nil {
		t.Fatalf("LoadPortMap returned error: %v", err)
	}

	if len(pm) != 1 {
		t.Fatalf("LoadPortMap returned %d ports, want 1", len(pm))
	}
	port := pm[4001]
	if port == nil {
		t.Fatal("LoadPortMap did not create port 4001")
	}
	if port.Addr != "127.0.0.1:4001" {
		t.Fatalf("port.Addr = %q, want %q", port.Addr, "127.0.0.1:4001")
	}
	cmd, cmdLen, _ := matchCommand(port, []byte{0xAA})
	if cmd == nil || cmdLen != 1 || len(cmd.Responses) != 1 || string(cmd.Responses[0]) != string([]byte{0xBB}) {
		t.Fatalf("command AA = %v length %d, want response BB", cmd, cmdLen)
	}
	cmd, cmdLen, _ = matchCommand(port, []byte{0xCC})
	if cmd == nil || cmdLen != 1 || len(cmd.Responses) != 1 || string(cmd.Responses[0]) != string([]byte{0xDD}) {
		t.Fatalf("command CC = %v length %d, want response DD", cmd, cmdLen)
	}
	if len(port.MergedRecords) != 1 || port.MergedRecords[0] != (MergedPortRecord{PortID: 2, Delay: 2000}) {
		t.Fatalf("MergedRecords = %+v, want duplicate port_id 2", port.MergedRecords)
	}
	if _, ok := pm[0]; ok {
		t.Fatal("LoadPortMap created a listener for port 0")
	}
	if _, ok := pm[70000]; ok {
		t.Fatal("LoadPortMap created a listener for port 70000")
	}
}

func TestLoadPortMapRejectsInvalidHexData(t *testing.T) {
	db := newTestDB(t)

	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 1, "10.0.0.8", 4001, 300)
	mustExec(t, db, "INSERT INTO commands (port_id, command_key, return_value, seq) VALUES (?, ?, ?, ?)", 1, "AA", "not-hex", 0)

	if _, err := LoadPortMap(db, "127.0.0.1"); err == nil {
		t.Fatal("LoadPortMap returned nil error for invalid return_value")
	}
}

func TestLoadPortMapRejectsNegativeDelay(t *testing.T) {
	db := newTestDB(t)

	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 1, "10.0.0.8", 4001, -1)

	if _, err := LoadPortMap(db, "127.0.0.1"); err == nil {
		t.Fatal("LoadPortMap returned nil error for negative delay")
	}
}

func TestLoadPortMapRejectsNegativeDelayOnMergedPort(t *testing.T) {
	db := newTestDB(t)

	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 1, "10.0.0.8", 4001, 300)
	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 2, "10.0.0.195", 4001, -1)

	if _, err := LoadPortMap(db, "127.0.0.1"); err == nil {
		t.Fatal("LoadPortMap returned nil error for negative delay on duplicate port")
	}
}

func TestLoadPortMapSkipsPortsWithoutCommands(t *testing.T) {
	db := newTestDB(t)

	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 1, "10.0.0.8", 4001, 300)
	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 2, "10.0.0.8", 4002, 300)
	mustExec(t, db, "INSERT INTO commands (port_id, command_key, return_value, seq) VALUES (?, ?, ?, ?)", 2, "AA", "BB", 0)

	pm, err := LoadPortMap(db, "127.0.0.1")
	if err != nil {
		t.Fatalf("LoadPortMap returned error: %v", err)
	}
	if len(pm) != 1 {
		t.Fatalf("LoadPortMap returned %d ports, want 1", len(pm))
	}
	if _, ok := pm[4001]; ok {
		t.Fatal("LoadPortMap kept a port without commands")
	}
	if _, ok := pm[4002]; !ok {
		t.Fatal("LoadPortMap skipped a port with commands")
	}
}

func TestLoadPortMapRejectsOverlongCommand(t *testing.T) {
	db := newTestDB(t)
	overlongCommand := make([]byte, maxCommandBytes+1)

	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 1, "10.0.0.8", 4001, 300)
	mustExec(t, db, "INSERT INTO commands (port_id, command_key, return_value, seq) VALUES (?, ?, ?, ?)", 1, BytesToHex(overlongCommand), "BB", 0)

	if _, err := LoadPortMap(db, "127.0.0.1"); err == nil {
		t.Fatal("LoadPortMap returned nil error for overlong command")
	}
}

func TestLoadPortSummariesSkipsInvalidPorts(t *testing.T) {
	db := newTestDB(t)

	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 1, "10.0.0.8", 4001, 300)
	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 2, "10.0.0.195", 4001, 2000)
	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 3, "10.0.0.8", 0, 60000)
	mustExec(t, db, "INSERT INTO ports (port_id, ip, port, delay) VALUES (?, ?, ?, ?)", 4, "10.0.0.8", 70000, 60000)

	summaries, err := LoadPortSummaries(db)
	if err != nil {
		t.Fatalf("LoadPortSummaries returned error: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("LoadPortSummaries returned %d summaries, want 1", len(summaries))
	}
	if summaries[0] != (PortSummary{Port: 4001, Items: 2}) {
		t.Fatalf("summary = %+v, want {Port:4001 Items:2}", summaries[0])
	}
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
	})

	mustExec(t, db, `CREATE TABLE ports (
		port_id INTEGER PRIMARY KEY,
		ip      TEXT    NOT NULL,
		port    INTEGER NOT NULL,
		delay   INTEGER NOT NULL DEFAULT 0
	)`)
	mustExec(t, db, `CREATE TABLE commands (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		port_id      INTEGER NOT NULL,
		command_key  TEXT    NOT NULL,
		return_value TEXT    NOT NULL,
		seq          INTEGER NOT NULL DEFAULT 0
	)`)

	return db
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
