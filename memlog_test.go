package memlog_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/benbjohnson/clock"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"
	"gotest.tools/v3/assert"

	"github.com/embano1/memlog"
)

func Test_Log_Checkpoint_Resume(t *testing.T) {
	const (
		sourceDataCount = 50
		start           = memlog.Offset(0)
		segSize         = 20
	)

	var (
		log *memlog.Log

		ctx        = context.Background()
		logr       = zaptest.NewLogger(t).Sugar()
		sourceData = memlog.NewTestDataSlice(t, sourceDataCount)
		checkpoint memlog.Offset
		records    []memlog.Record
	)

	t.Run("create log", func(t *testing.T) {
		opts := []memlog.Option{
			memlog.WithClock(clock.NewMock()),
			memlog.WithStartOffset(start),
			memlog.WithMaxSegmentSize(segSize),
		}

		l, err := memlog.New(ctx, opts...)
		assert.NilError(t, err)
		log = l
	})

	t.Run("writes 20 records", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			offset, err := log.Write(ctx, sourceData[i])
			assert.NilError(t, err)
			assert.Equal(t, offset, memlog.Offset(i))
		}
	})

	t.Run("reads 20 records, creates checkpoint at 10", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			r, err := log.Read(ctx, memlog.Offset(i))
			assert.NilError(t, err)
			assert.Equal(t, r.Metadata.Offset, memlog.Offset(i))
			records = append(records, r)

			if r.Metadata.Offset == 10 {
				checkpoint = r.Metadata.Offset
				logr.Infow("checkpoint created", "offset", checkpoint)
			}
		}
	})

	t.Run("log crashes, writes 20 records starting from last checkpoint", func(t *testing.T) {
		log = nil

		opts := []memlog.Option{
			memlog.WithClock(clock.NewMock()),
			memlog.WithStartOffset(checkpoint),
			memlog.WithMaxSegmentSize(segSize),
		}

		l, err := memlog.New(ctx, opts...)
		assert.NilError(t, err)
		log = l

		for i := checkpoint; i < checkpoint+20; i++ {
			offset, writeErr := log.Write(ctx, sourceData[i])
			assert.NilError(t, writeErr)
			assert.Equal(t, offset, i)
		}
	})

	t.Run("reader resumes, catches up until no new data, creates checkpoint at last successful record", func(t *testing.T) {
		for i := checkpoint; ; i++ {
			r, err := log.Read(ctx, i)
			if err != nil {
				assert.Assert(t, errors.Is(err, memlog.ErrFutureOffset))
				checkpoint = memlog.Offset(i) - 1 // last successful read
				logr.Infow("checkpoint created", "offset", checkpoint)
				break
			}

			assert.Equal(t, r.Metadata.Offset, memlog.Offset(i))
			records = append(records, r)
		}
	})

	t.Run("continue writes, records purged", func(t *testing.T) {
		for i := int(checkpoint); i < len(sourceData); i++ {
			_, err := log.Write(ctx, sourceData[i])
			assert.NilError(t, err)
		}
	})

	t.Run("slow reader attempts to read purged record", func(t *testing.T) {
		_, err := log.Read(ctx, checkpoint)
		assert.Assert(t, errors.Is(err, memlog.ErrOutOfRange))
	})

	t.Run("retrieve earliest and latest, read until end", func(t *testing.T) {
		earliest, latest := log.Range(ctx)

		for i := earliest; i <= latest; i++ {
			r, err := log.Read(ctx, i)
			assert.NilError(t, err)
			records = append(records, r)
		}
	})

	t.Run("all records received", func(t *testing.T) {
		d := dedupe(t, records)
		assert.Equal(t, len(d), len(sourceData))

		for i, r := range d {
			assert.DeepEqual(t, r.Data, sourceData[i])
		}
	})
}

func Test_Log_Concurrent(t *testing.T) {
	type wantOffsets struct {
		earliest memlog.Offset
		latest   memlog.Offset
	}
	testCases := []struct {
		name    string
		start   memlog.Offset
		segSize int
		worker  int
		want    wantOffsets
	}{
		{
			name:    "100 workers, starts at 0, no purge",
			start:   0,
			segSize: 100,
			worker:  100,
			want: wantOffsets{
				earliest: 0,
				latest:   99,
			},
		},
		{
			name:    "100 workers, starts at 100, with purge",
			start:   100,
			segSize: 10,
			worker:  50,
			want: wantOffsets{
				earliest: 130,
				latest:   149,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			opts := []memlog.Option{
				memlog.WithStartOffset(tc.start),
				memlog.WithMaxSegmentSize(tc.segSize),
			}

			l, err := memlog.New(ctx, opts...)
			assert.NilError(t, err)

			eg, egCtx := errgroup.WithContext(ctx)
			testData := memlog.NewTestDataSlice(t, tc.worker)

			for i := 0; i < tc.worker; i++ {
				data := testData[i]
				eg.Go(func() error {
					offset, writeErr := l.Write(egCtx, data)
					assert.Assert(t, offset != -1)

					// assert earliest/latest never return invalid offsets
					earliest, latest := l.Range(ctx)
					assert.Assert(t, earliest != memlog.Offset(-1))
					assert.Assert(t, latest != memlog.Offset(-1))
					return writeErr
				})
			}

			err = eg.Wait()
			assert.NilError(t, err)

			earliest, latest := l.Range(ctx)
			assert.Equal(t, earliest, tc.want.earliest)
			assert.Equal(t, latest, tc.want.latest)
		})
	}
}

func dedupe(t *testing.T, records []memlog.Record) []memlog.Record {
	t.Helper()

	type dataSchema struct {
		ID string `json:"id"`
	}

	var (
		deduped []memlog.Record
		d       dataSchema
	)

	seen := make(map[string]struct{})
	for _, r := range records {
		err := json.Unmarshal(r.Data, &d)
		assert.NilError(t, err)

		if _, ok := seen[d.ID]; !ok {
			seen[d.ID] = struct{}{}
			deduped = append(deduped, r)
		}
	}

	return deduped
}
