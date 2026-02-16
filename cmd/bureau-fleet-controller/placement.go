// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path"
	"sort"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// ineligible is the sentinel score returned when a machine cannot host a
// service. Any negative score means "do not place here."
const ineligible = -1

// placementCandidate pairs a machine localpart with its placement score.
// Returned by scorePlacement in descending score order.
type placementCandidate struct {
	machineLocalpart string
	score            int
}

// scoreMachine evaluates a single candidate machine for hosting a service.
// Returns ineligible (-1) if the machine cannot host the service, or a
// non-negative score (higher is better). The score is scaled to a 0-100000
// range so that integer arithmetic preserves sufficient precision without
// floating point.
//
// The function is pure: it reads fc.machines and fc.services but performs
// no I/O and has no side effects.
func (fc *FleetController) scoreMachine(machineLocalpart string, service *schema.FleetServiceContent) int {
	machine, exists := fc.machines[machineLocalpart]
	if !exists {
		return ineligible
	}

	// A machine without hardware info cannot be evaluated — we don't
	// know its capacity, labels, or GPU inventory.
	if machine.info == nil {
		return ineligible
	}

	// A machine without status cannot be evaluated — we don't know its
	// current utilization or whether it's even online.
	if machine.status == nil {
		return ineligible
	}

	// Cordoned machines are explicitly removed from scheduling.
	if _, cordoned := machine.info.Labels["cordoned"]; cordoned {
		return ineligible
	}

	// Every required label must be present. Labels are either "key"
	// (presence check) or "key=value" (exact match).
	for _, requirement := range service.Placement.Requires {
		key, value, hasValue := strings.Cut(requirement, "=")
		machineValue, present := machine.info.Labels[key]
		if !present {
			return ineligible
		}
		if hasValue && machineValue != value {
			return ineligible
		}
	}

	// AllowedMachines restricts which machines are candidates. If the
	// list is non-empty, the machine localpart must match at least one
	// glob pattern.
	if len(service.Placement.AllowedMachines) > 0 {
		allowed := false
		for _, pattern := range service.Placement.AllowedMachines {
			matched, err := path.Match(pattern, machineLocalpart)
			if err != nil {
				// path.Match returns an error only for malformed
				// patterns. Treat malformed patterns as non-matching
				// rather than silently allowing all machines.
				continue
			}
			if matched {
				allowed = true
				break
			}
		}
		if !allowed {
			return ineligible
		}
	}

	// Memory capacity check.
	availableMemoryMB := machine.info.MemoryTotalMB - machine.status.MemoryUsedMB
	if service.Resources.MemoryMB > 0 && availableMemoryMB < service.Resources.MemoryMB {
		return ineligible
	}

	// GPU presence check.
	if service.Resources.GPU && len(machine.info.GPUs) == 0 {
		return ineligible
	}

	// GPU memory capacity check. Sum total VRAM across all GPUs,
	// subtract used VRAM from status, compare against requirement.
	if service.Resources.GPUMemoryMB > 0 {
		totalVRAMBytes := int64(0)
		for index := range machine.info.GPUs {
			totalVRAMBytes += machine.info.GPUs[index].VRAMTotalBytes
		}
		usedVRAMBytes := int64(0)
		for index := range machine.status.GPUStats {
			usedVRAMBytes += machine.status.GPUStats[index].VRAMUsedBytes
		}
		availableVRAMMB := (totalVRAMBytes - usedVRAMBytes) / (1024 * 1024)
		if availableVRAMMB < int64(service.Resources.GPUMemoryMB) {
			return ineligible
		}
	}

	// Anti-affinity: the machine must not already host any service
	// listed in AntiAffinity.
	for _, antiAffinityLocalpart := range service.Placement.AntiAffinity {
		if _, assigned := machine.assignments[antiAffinityLocalpart]; assigned {
			return ineligible
		}
	}

	// --- Scoring ---
	// Each component produces a 0-100 value. Components are multiplied
	// by their weight, summed, then divided by total weight to produce
	// a 0-100 composite. This composite is then scaled by a time-of-day
	// factor (1000 for always-on, 300 or 1000 for batch depending on
	// window match) to give a 0-100000 integer score.

	totalMemoryMB := machine.info.MemoryTotalMB

	// Memory headroom: more free memory = higher score.
	memoryHeadroomScore := 0
	if totalMemoryMB > 0 {
		memoryHeadroomScore = availableMemoryMB * 100 / totalMemoryMB
		if memoryHeadroomScore > 100 {
			memoryHeadroomScore = 100
		}
	}

	// CPU headroom: less CPU load = higher score.
	cpuHeadroomScore := 100 - machine.status.CPUPercent
	if cpuHeadroomScore < 0 {
		cpuHeadroomScore = 0
	}

	// GPU headroom: only scored when the service requires a GPU.
	gpuHeadroomScore := 0
	gpuWeight := 0
	if service.Resources.GPU {
		gpuWeight = 20
		if len(machine.status.GPUStats) > 0 {
			totalUtilization := 0
			for index := range machine.status.GPUStats {
				totalUtilization += machine.status.GPUStats[index].UtilizationPercent
			}
			averageUtilization := totalUtilization / len(machine.status.GPUStats)
			gpuHeadroomScore = 100 - averageUtilization
			if gpuHeadroomScore < 0 {
				gpuHeadroomScore = 0
			}
		} else if len(machine.info.GPUs) > 0 {
			// GPUs exist but no stats available — neutral score.
			gpuHeadroomScore = 50
		}
	}

	// Preferred machine bonus: first = 100, second = 66, third = 33.
	preferredScore := 0
	for index, preferred := range service.Placement.PreferredMachines {
		if preferred == machineLocalpart {
			switch index {
			case 0:
				preferredScore = 100
			case 1:
				preferredScore = 66
			case 2:
				preferredScore = 33
			}
			break
		}
	}

	// Co-locate bonus: proportional to how many co-locate targets are
	// already on this machine.
	coLocateScore := 0
	if len(service.Placement.CoLocateWith) > 0 {
		coLocatedCount := 0
		for _, coLocateLocalpart := range service.Placement.CoLocateWith {
			if _, assigned := machine.assignments[coLocateLocalpart]; assigned {
				coLocatedCount++
			}
		}
		coLocateScore = coLocatedCount * 100 / len(service.Placement.CoLocateWith)
	}

	// Utilization penalty: same formula as CPU headroom — double-
	// penalizes high utilization through a separate weight.
	utilizationPenaltyScore := 100 - machine.status.CPUPercent
	if utilizationPenaltyScore < 0 {
		utilizationPenaltyScore = 0
	}

	// Weighted sum. Total weight adjusts based on whether GPU scoring
	// is active: 25 + 25 + gpuWeight + 15 + 10 + 5.
	totalWeight := 25 + 25 + gpuWeight + 15 + 10 + 5
	weightedSum := memoryHeadroomScore*25 +
		cpuHeadroomScore*25 +
		gpuHeadroomScore*gpuWeight +
		preferredScore*15 +
		coLocateScore*10 +
		utilizationPenaltyScore*5

	compositeScore := weightedSum / totalWeight

	// Time-of-day scaling for batch services.
	if service.Scheduling != nil &&
		service.Scheduling.Class == "batch" &&
		len(service.Scheduling.PreferredWindows) > 0 {
		now := fc.clock.Now()
		inWindow := false
		for _, window := range service.Scheduling.PreferredWindows {
			if inTimeWindow(now, window) {
				inWindow = true
				break
			}
		}
		if inWindow {
			return compositeScore * 1000
		}
		return compositeScore * 300
	}

	return compositeScore * 1000
}

// scorePlacement returns a scored list of all eligible machines for a
// service, sorted by score descending. Ties are broken by machine
// localpart (lexicographic, ascending) for determinism.
func (fc *FleetController) scorePlacement(service *schema.FleetServiceContent) []placementCandidate {
	var candidates []placementCandidate

	for machineLocalpart := range fc.machines {
		score := fc.scoreMachine(machineLocalpart, service)
		if score == ineligible {
			continue
		}
		candidates = append(candidates, placementCandidate{
			machineLocalpart: machineLocalpart,
			score:            score,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].machineLocalpart < candidates[j].machineLocalpart
	})

	return candidates
}

// inTimeWindow checks whether the given time falls within a TimeWindow,
// considering day-of-week and hour ranges. Handles midnight wrapping:
// a window with StartHour=22 and EndHour=6 means 10 PM to 6 AM.
func inTimeWindow(now time.Time, window schema.TimeWindow) bool {
	// Day-of-week check. If Days is non-empty, the current day must
	// match at least one entry.
	if len(window.Days) > 0 {
		dayAbbreviation := strings.ToLower(now.Weekday().String()[:3])
		dayMatches := false
		for _, day := range window.Days {
			if strings.ToLower(day) == dayAbbreviation {
				dayMatches = true
				break
			}
		}
		if !dayMatches {
			return false
		}
	}

	hour := now.Hour()

	if window.StartHour < window.EndHour {
		// Normal range: e.g., 9-17 means 9:00 to 16:59.
		return hour >= window.StartHour && hour < window.EndHour
	}

	if window.StartHour > window.EndHour {
		// Midnight-wrapping range: e.g., 22-6 means 22:00 to 05:59.
		return hour >= window.StartHour || hour < window.EndHour
	}

	// StartHour == EndHour: the window spans the full 24 hours (a
	// start and end at the same hour means "all day").
	return true
}
