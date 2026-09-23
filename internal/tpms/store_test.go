package tpms

import (
	"testing"
	"time"
)

// arrive simulates a vehicle passing: each of its sensors transmits a
// few seconds apart, the way wheels wake up one after another.
func arrive(s *Store, at time.Time, model string, ids ...string) {
	for i, id := range ids {
		s.Add(Reading{
			At:    at.Add(time.Duration(i) * 3 * time.Second),
			Model: model, ID: id,
			PressureKPa: 220, HasPressure: true,
			TemperatureC: 20, HasTemperature: true,
			Integrity: "CRC",
		})
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestClustersOneVehicle is the core case: four sensors that keep
// arriving together should be recognised as one car.
func TestClustersOneVehicle(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().Add(-24 * time.Hour)
	for i := range 4 { // four separate trips, hours apart
		arrive(s, base.Add(time.Duration(i)*3*time.Hour), "Toyota", "aa01", "aa02", "aa03", "aa04")
	}

	sensors, vehicles, total := s.Snapshot()
	if total != 16 {
		t.Errorf("total readings = %d, want 16", total)
	}
	if len(sensors) != 4 {
		t.Fatalf("sensors = %d, want 4", len(sensors))
	}
	if len(vehicles) != 1 {
		t.Fatalf("vehicles = %d, want 1: %+v", len(vehicles), vehicles)
	}
	if len(vehicles[0].Sensors) != 4 {
		t.Errorf("vehicle has %d sensors, want 4", len(vehicles[0].Sensors))
	}
	for _, sen := range sensors {
		if sen.Vehicle != vehicles[0].ID {
			t.Errorf("sensor %s not assigned to the vehicle", sen.Key)
		}
	}
}

// TestKeepsTwoVehiclesApart guards the main failure mode: two cars that
// occasionally overlap must not be merged into one.
func TestKeepsTwoVehiclesApart(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().Add(-48 * time.Hour)

	for i := range 6 {
		arrive(s, base.Add(time.Duration(i)*5*time.Hour), "Toyota", "aa01", "aa02", "aa03", "aa04")
		// The second car arrives four hours later — well outside the
		// co-occurrence window.
		arrive(s, base.Add(time.Duration(i)*5*time.Hour+4*time.Hour), "Ford", "bb01", "bb02", "bb03", "bb04")
	}
	// Twice they happen to arrive together.
	for i := range 2 {
		at := base.Add(time.Duration(i)*time.Hour + 90*time.Hour)
		arrive(s, at, "Toyota", "aa01", "aa02", "aa03", "aa04")
		arrive(s, at.Add(20*time.Second), "Ford", "bb01", "bb02", "bb03", "bb04")
	}

	_, vehicles, _ := s.Snapshot()
	if len(vehicles) != 2 {
		t.Fatalf("vehicles = %d, want 2 (the cars were merged): %+v", len(vehicles), vehicles)
	}
	for _, v := range vehicles {
		if len(v.Sensors) != 4 {
			t.Errorf("vehicle %s has %d sensors, want 4", v.ID, len(v.Sensors))
		}
	}
}

// TestLoneSensorIsNotAVehicle covers a car heard once in passing.
func TestLoneSensorIsNotAVehicle(t *testing.T) {
	s := newTestStore(t)
	s.Add(Reading{At: time.Now(), Model: "Schrader", ID: "ff99", PressureKPa: 210, HasPressure: true})

	sensors, vehicles, _ := s.Snapshot()
	if len(sensors) != 1 {
		t.Fatalf("sensors = %d, want 1", len(sensors))
	}
	if len(vehicles) != 0 {
		t.Errorf("vehicles = %d, want 0 for a single sighting", len(vehicles))
	}
	if sensors[0].Vehicle != "" {
		t.Error("lone sensor should not be assigned to a vehicle")
	}
}

// TestOneOffOverlapDoesNotLink checks the MinShared floor: sensors that
// coincide once must not be grouped.
func TestOneOffOverlapDoesNotLink(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	arrive(s, now, "Toyota", "aa01", "aa02")
	arrive(s, now.Add(10*time.Second), "Ford", "bb01", "bb02")

	_, vehicles, _ := s.Snapshot()
	if len(vehicles) != 0 {
		t.Errorf("a single coincidence produced %d vehicles, want 0", len(vehicles))
	}
}

// TestSameIDDifferentDecoders keeps two protocols that happen to emit
// the same bare ID from being treated as one sensor.
func TestSameIDDifferentDecoders(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	s.Add(Reading{At: now, Model: "Toyota", ID: "1234"})
	s.Add(Reading{At: now, Model: "Ford", ID: "1234"})

	sensors, _, _ := s.Snapshot()
	if len(sensors) != 2 {
		t.Errorf("sensors = %d, want 2 distinct despite the shared id", len(sensors))
	}
}

func TestLabelsPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-10 * time.Hour)
	for i := range 4 {
		arrive(s, base.Add(time.Duration(i)*2*time.Hour), "Toyota", "aa01", "aa02", "aa03", "aa04")
	}
	_, vehicles, _ := s.Snapshot()
	if len(vehicles) != 1 {
		t.Fatalf("expected 1 vehicle to label, got %d", len(vehicles))
	}
	s.SetLabel("vehicle", vehicles[0].ID, "Rob's Subaru")
	s.SetLabel("sensor", "Toyota/aa01", "front left")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: sensors, counts, clustering and labels must all come back.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	sensors, vehicles2, _ := s2.Snapshot()
	if len(sensors) != 4 {
		t.Errorf("after reload: %d sensors, want 4", len(sensors))
	}
	if len(vehicles2) != 1 {
		t.Fatalf("after reload: %d vehicles, want 1", len(vehicles2))
	}
	if vehicles2[0].Label != "Rob's Subaru" {
		t.Errorf("vehicle label = %q, want \"Rob's Subaru\"", vehicles2[0].Label)
	}
	var found bool
	for _, sen := range sensors {
		if sen.Key == "Toyota/aa01" {
			found = sen.Label == "front left"
		}
	}
	if !found {
		t.Error("sensor label did not survive the reload")
	}
}

func TestReadingLogIsAppended(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	arrive(s, time.Now(), "Toyota", "aa01", "aa02")
	s.Close()

	b, err := readLines(dir + "/readings.jsonl")
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	if len(b) != 2 {
		t.Errorf("logged %d readings, want 2", len(b))
	}
}

// vehicleOf finds which vehicle a sensor ended up in, or "" for none.
func vehicleOf(t *testing.T, s *Store, key string) string {
	t.Helper()
	sensors, _, _ := s.Snapshot()
	for _, sen := range sensors {
		if sen.Key == key {
			return sen.Vehicle
		}
	}
	t.Fatalf("no sensor %q", key)
	return ""
}

// Two cars that keep passing together get merged, and no amount of
// further listening separates them — co-occurrence only ever accumulates.
// So there has to be a way to say they are not the same car.
func TestSensorCanBeTakenOutOfTheWrongVehicle(t *testing.T) {
	s := newTestStore(t)

	// A car and a motorbike that always travel together, often enough to
	// be indistinguishable from one vehicle.
	at := time.Now().Add(-time.Hour)
	for i := range 6 {
		arrive(s, at.Add(time.Duration(i)*10*time.Minute), "Toyota", "aa01", "aa02", "bb01")
	}

	_, vehicles, _ := s.Snapshot()
	if len(vehicles) != 1 {
		t.Fatalf("expected the three to be merged, got %d vehicles", len(vehicles))
	}
	merged := vehicles[0].ID

	// Pull the odd one out.
	s.SetVehicle("Toyota/bb01", Alone)

	sensors, vehicles, _ := s.Snapshot()
	if got := vehicleOf(t, s, "Toyota/bb01"); got == merged {
		t.Errorf("bb01 is still in %s after being taken out of it", got)
	}
	// and the other two are left where they were
	if a, b := vehicleOf(t, s, "Toyota/aa01"), vehicleOf(t, s, "Toyota/aa02"); a != b || a == "" {
		t.Errorf("the remaining pair came apart: %q and %q", a, b)
	}
	_ = sensors
	_ = vehicles
}

// Moving a sensor into a named vehicle is how a cluster that split, or
// never formed, gets put right.
func TestSensorCanBeAssignedToAVehicle(t *testing.T) {
	s := newTestStore(t)
	at := time.Now().Add(-time.Hour)
	for i := range 5 {
		arrive(s, at.Add(time.Duration(i)*10*time.Minute), "Toyota", "aa01", "aa02")
	}
	// A wheel heard on its own, never at the same time as the others.
	arrive(s, at.Add(9*time.Hour), "Toyota", "cc01")

	_, vehicles, _ := s.Snapshot()
	if len(vehicles) != 1 {
		t.Fatalf("expected one vehicle, got %d", len(vehicles))
	}
	target := vehicles[0].ID

	if got := vehicleOf(t, s, "Toyota/cc01"); got != "" {
		t.Fatalf("cc01 started in %q, expected it to be unassigned", got)
	}
	s.SetVehicle("Toyota/cc01", target)

	if got := vehicleOf(t, s, "Toyota/cc01"); got != target {
		t.Errorf("cc01 is in %q, want %q", got, target)
	}
	_, vehicles, _ = s.Snapshot()
	for _, v := range vehicles {
		if v.ID == target && len(v.Sensors) != 3 {
			t.Errorf("%s has %d sensors, want 3", target, len(v.Sensors))
		}
	}
}

// Clearing an assignment hands the sensor back to the clustering.
func TestClearingAnAssignmentRestoresClustering(t *testing.T) {
	s := newTestStore(t)
	at := time.Now().Add(-time.Hour)
	for i := range 6 {
		arrive(s, at.Add(time.Duration(i)*10*time.Minute), "Toyota", "aa01", "aa02")
	}
	was := vehicleOf(t, s, "Toyota/aa02")

	s.SetVehicle("Toyota/aa02", Alone)
	if vehicleOf(t, s, "Toyota/aa02") == was {
		t.Fatal("the sensor was not taken out")
	}

	s.SetVehicle("Toyota/aa02", "")
	if got := vehicleOf(t, s, "Toyota/aa02"); got != was {
		t.Errorf("after clearing, the sensor is in %q, want %q", got, was)
	}
}

// An assignment has to outlive a restart, or it is not worth making.
func TestAssignmentsSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	at := time.Now().Add(-time.Hour)
	for i := range 6 {
		arrive(s, at.Add(time.Duration(i)*10*time.Minute), "Toyota", "aa01", "aa02", "bb01")
	}
	s.SetVehicle("Toyota/bb01", Alone)
	apart := vehicleOf(t, s, "Toyota/bb01")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if got := vehicleOf(t, again, "Toyota/bb01"); got != apart {
		t.Errorf("after reloading, bb01 is in %q, want %q", got, apart)
	}
	if got := again.Pinned("Toyota/bb01"); got != Alone {
		t.Errorf("the assignment did not survive: %q", got)
	}
}
