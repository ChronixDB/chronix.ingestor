// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ingester

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/prometheus/common/log"

	"github.com/prometheus/common/model"
)

const maxMappedFP = 1 << 20 // About 1M fingerprints reserved for mapping.

var separatorString = string([]byte{model.SeparatorByte})

// fpMappings maps original fingerprints to a map of string representations of
// metrics to the truly unique fingerprint.
type fpMappings map[model.Fingerprint]map[string]model.Fingerprint

// fpMapper is used to map fingerprints in order to work around fingerprint
// collisions.
type fpMapper struct {
	// highestMappedFP has to be aligned for atomic operations.
	highestMappedFP model.Fingerprint

	mtx      sync.RWMutex // Protects mappings.
	mappings fpMappings

	fpToSeries *seriesMap
}

// newFPMapper loads the collision map from the persistence and
// returns an fpMapper ready to use.
func newFPMapper(fpToSeries *seriesMap) *fpMapper {
	return &fpMapper{
		fpToSeries: fpToSeries,
		mappings:   map[model.Fingerprint]map[string]model.Fingerprint{},
	}
}

// mapFP takes a raw fingerprint (as returned by Metrics.FastFingerprint) and
// returns a truly unique fingerprint. The caller must have locked the raw
// fingerprint.
//
// If an error is encountered, it is returned together with the unchanged raw
// fingerprint.
func (m *fpMapper) mapFP(fp model.Fingerprint, metric model.Metric) model.Fingerprint {
	// First check if we are in the reserved FP space, in which case this is
	// automatically a collision that has to be mapped.
	if fp <= maxMappedFP {
		return m.maybeAddMapping(fp, metric)
	}

	// Then check the most likely case: This fp belongs to a series that is
	// already in memory.
	s, ok := m.fpToSeries.get(fp)
	if ok {
		// FP exists in memory, but is it for the same metric?
		if metric.Equal(s.metric) {
			// Yupp. We are done.
			return fp
		}
		// Collision detected!
		return m.maybeAddMapping(fp, metric)
	}
	// Metric is not in memory. Before doing the expensive archive lookup,
	// check if we have a mapping for this metric in place already.
	m.mtx.RLock()
	mappedFPs, fpAlreadyMapped := m.mappings[fp]
	m.mtx.RUnlock()
	if fpAlreadyMapped {
		// We indeed have mapped fp historically.
		ms := metricToUniqueString(metric)
		// fp is locked by the caller, so no further locking of
		// 'collisions' required (it is specific to fp).
		mappedFP, ok := mappedFPs[ms]
		if ok {
			// Historical mapping found, return the mapped FP.
			return mappedFP
		}
	}
	return fp
}

// maybeAddMapping is only used internally. It takes a detected collision and
// adds it to the collisions map if not yet there. In any case, it returns the
// truly unique fingerprint for the colliding metric.
func (m *fpMapper) maybeAddMapping(
	fp model.Fingerprint,
	collidingMetric model.Metric,
) model.Fingerprint {
	ms := metricToUniqueString(collidingMetric)
	m.mtx.RLock()
	mappedFPs, ok := m.mappings[fp]
	m.mtx.RUnlock()
	if ok {
		// fp is locked by the caller, so no further locking required.
		mappedFP, ok := mappedFPs[ms]
		if ok {
			return mappedFP // Existing mapping.
		}
		// A new mapping has to be created.
		mappedFP = m.nextMappedFP()
		mappedFPs[ms] = mappedFP
		log.Infof(
			"Collision detected for fingerprint %v, metric %v, mapping to new fingerprint %v.",
			fp, collidingMetric, mappedFP,
		)
		return mappedFP
	}
	// This is the first collision for fp.
	mappedFP := m.nextMappedFP()
	mappedFPs = map[string]model.Fingerprint{ms: mappedFP}
	m.mtx.Lock()
	m.mappings[fp] = mappedFPs
	m.mtx.Unlock()
	log.Infof(
		"Collision detected for fingerprint %v, metric %v, mapping to new fingerprint %v.",
		fp, collidingMetric, mappedFP,
	)
	return mappedFP
}

func (m *fpMapper) nextMappedFP() model.Fingerprint {
	mappedFP := model.Fingerprint(atomic.AddUint64((*uint64)(&m.highestMappedFP), 1))
	if mappedFP > maxMappedFP {
		panic(fmt.Errorf("more than %v fingerprints mapped in collision detection", maxMappedFP))
	}
	return mappedFP
}

// metricToUniqueString turns a metric into a string in a reproducible and
// unique way, i.e. the same metric will always create the same string, and
// different metrics will always create different strings. In a way, it is the
// "ideal" fingerprint function, only that it is more expensive than the
// FastFingerprint function, and its result is not suitable as a key for maps
// and indexes as it might become really large, causing a lot of hashing effort
// in maps and a lot of storage overhead in indexes.
func metricToUniqueString(m model.Metric) string {
	parts := make([]string, 0, len(m))
	for ln, lv := range m {
		parts = append(parts, string(ln)+separatorString+string(lv))
	}
	sort.Strings(parts)
	return strings.Join(parts, separatorString)
}
