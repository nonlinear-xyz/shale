package watch

import "testing"

// The clamp is the difference between "one file failed, retry next sweep" and
// "one file failed and is now invisible forever". Directory order is not mtime
// order, so a failure is routinely followed by a NEWER success that would drag
// the watermark past it.
func TestClamp(t *testing.T) {
	const noFailure = int64(-1)

	cases := []struct {
		name                            string
		startCursor, maxSeen, minFailed int64
		want                            int64
	}{
		{
			name:        "clean sweep advances to the newest file seen",
			startCursor: 100, maxSeen: 500, minFailed: noFailure, want: 500,
		},
		{
			// The regression this exists for: file A (mtime 200) fails, file B
			// (mtime 500) succeeds afterwards because readdir returned it later.
			// Without the clamp the cursor lands at 500 and A is never offered again.
			name:        "a newer success must not drag the watermark past an older failure",
			startCursor: 100, maxSeen: 500, minFailed: 200, want: 199,
		},
		{
			name:        "failure older than the starting cursor cannot regress it",
			startCursor: 300, maxSeen: 500, minFailed: 200, want: 300,
		},
		{
			name:        "failure newer than everything seen leaves the watermark alone",
			startCursor: 100, maxSeen: 400, minFailed: 900, want: 400,
		},
		{
			name:        "nothing new seen holds the cursor steady",
			startCursor: 100, maxSeen: 100, minFailed: noFailure, want: 100,
		},
		{
			name:        "every file failed — cursor must not move at all",
			startCursor: 100, maxSeen: 100, minFailed: 100, want: 100,
		},
		{
			name:        "first ever sweep with a failure starts from zero",
			startCursor: 0, maxSeen: 800, minFailed: 50, want: 49,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Clamp(tc.startCursor, tc.maxSeen, tc.minFailed)
			if got != tc.want {
				t.Errorf("Clamp(start=%d, maxSeen=%d, minFailed=%d) = %d, want %d",
					tc.startCursor, tc.maxSeen, tc.minFailed, got, tc.want)
			}
		})
	}
}

// The watermark may never move backwards. A regression would re-offer every
// settled session on the next sweep, and since a failing file holds the cursor,
// the same order would repeat — turning one bad transcript into an endless loop.
func TestClampNeverRegressesBelowStart(t *testing.T) {
	for _, maxSeen := range []int64{0, 1, 50, 99} {
		for _, minFailed := range []int64{-1, 0, 1, 50, 100, 5000} {
			if got := Clamp(100, maxSeen, minFailed); got < 100 {
				t.Fatalf("Clamp(100, %d, %d) = %d — regressed below the starting cursor",
					maxSeen, minFailed, got)
			}
		}
	}
}
