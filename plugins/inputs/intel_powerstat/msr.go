//go:build linux

package intel_powerstat

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/influxdata/telegraf"
)

const (
	systemCPUPath                      = "/sys/devices/system/cpu/"
	cpuCurrentFreqPartialPath          = "/sys/devices/system/cpu/cpu%s/cpufreq/scaling_cur_freq"
	msrPartialPath                     = "/dev/cpu/%s/msr"
	uncoreFreqPath                     = "/sys/devices/system/cpu/intel_uncore_frequency/package_%s_die_%s/%s%s_freq_khz"
	c3StateResidencyLocation           = 0x3FC
	c6StateResidencyLocation           = 0x3FD
	c7StateResidencyLocation           = 0x3FE
	maximumFrequencyClockCountLocation = 0xE7
	actualFrequencyClockCountLocation  = 0xE8
	throttleTemperatureLocation        = 0x1A2
	temperatureLocation                = 0x19C
	timestampCounterLocation           = 0x10
	turboRatioLimitLocation            = 0x1AD
	turboRatioLimit1Location           = 0x1AE
	turboRatioLimit2Location           = 0x1AF
	atomCoreTurboRatiosLocation        = 0x66C
	uncorePerfStatusLocation           = 0x621
	platformInfo                       = 0xCE
	fsbFreq                            = 0xCD
)
const (
	msrTurboRatioLimitString     = "MSR_TURBO_RATIO_LIMIT"
	msrTurboRatioLimit1String    = "MSR_TURBO_RATIO_LIMIT1"
	msrTurboRatioLimit2String    = "MSR_TURBO_RATIO_LIMIT2"
	msrAtomCoreTurboRatiosString = "MSR_ATOM_CORE_TURBO_RATIOS"
	msrUncorePerfStatusString    = "MSR_UNCORE_PERF_STATUS"
	msrPlatformInfoString        = "MSR_PLATFORM_INFO"
	msrFSBFreqString             = "MSR_FSB_FREQ"
)

// Maximum size of core IDs or socket IDs (8192). Based on maximum value of CPUs that linux kernel supports.
const maxIDsSize = 1 << 13

// msrService is responsible for interactions with MSR.
type msrService interface {
	getCPUCoresData() map[string]*msrData
	retrieveCPUFrequencyForCore(core string) (float64, error)
	retrieveUncoreFrequency(socketID string, typeFreq string, kind string, die string) (float64, error)
	openAndReadMsr(core string) error
	readSingleMsr(core string, msr string) (uint64, error)
	isMsrLoaded() bool
}

type msrServiceImpl struct {
	cpuCoresData map[string]*msrData
	cpuCores     []int
	msrOffsets   []int64
	fs           fileService
	log          telegraf.Logger
}

func (m *msrServiceImpl) getCPUCoresData() map[string]*msrData {
	return m.cpuCoresData
}

func (m *msrServiceImpl) isMsrLoaded() bool {
	for cpuID := range m.getCPUCoresData() {
		err := m.openAndReadMsr(cpuID)
		if err == nil {
			return true
		}
	}
	return false
}
func (m *msrServiceImpl) retrieveCPUFrequencyForCore(core string) (float64, error) {
	cpuFreqPath := fmt.Sprintf(cpuCurrentFreqPartialPath, core)
	err := checkFile(cpuFreqPath)
	if err != nil {
		return 0, err
	}
	cpuFreqFile, err := os.Open(cpuFreqPath)
	if err != nil {
		return 0, fmt.Errorf("error opening scaling_cur_freq file on path %q: %w", cpuFreqPath, err)
	}
	defer cpuFreqFile.Close()

	cpuFreq, _, err := m.fs.readFileToFloat64(cpuFreqFile)
	return convertKiloHertzToMegaHertz(cpuFreq), err
}

func (m *msrServiceImpl) retrieveUncoreFrequency(socketID string, typeFreq string, kind string, die string) (float64, error) {
	uncoreFreqPath, err := createUncoreFreqPath(socketID, typeFreq, kind, die)
	if err != nil {
		return 0, fmt.Errorf("unable to create uncore freq read path for socketID %q, and frequency type %q: %w", socketID, typeFreq, err)
	}
	err = checkFile(uncoreFreqPath)
	if err != nil {
		return 0, err
	}
	uncoreFreqFile, err := os.Open(uncoreFreqPath)
	if err != nil {
		return 0, fmt.Errorf("error opening uncore frequncy file on %q: %w", uncoreFreqPath, err)
	}
	defer uncoreFreqFile.Close()

	uncoreFreq, _, err := m.fs.readFileToFloat64(uncoreFreqFile)
	return convertKiloHertzToMegaHertz(uncoreFreq), err
}

func createUncoreFreqPath(socketID string, typeFreq string, kind string, die string) (string, error) {
	if socketID >= "0" && socketID <= "9" {
		socketID = fmt.Sprintf("0%s", socketID)
	}
	if die >= "0" && die <= "9" {
		die = fmt.Sprintf("0%s", die)
	}
	var prefix string

	switch typeFreq {
	case "initial":
		prefix = "initial_"
	case "current":
		prefix = ""
	default:
		return "", fmt.Errorf("unknown frequency type %s, only 'initial' and 'current' are supported", typeFreq)
	}

	if kind != "min" && kind != "max" {
		return "", fmt.Errorf("unknown frequency type %s, only 'min' and 'max' are supported", kind)
	}
	return fmt.Sprintf(uncoreFreqPath, socketID, die, prefix, kind), nil
}

func (m *msrServiceImpl) openAndReadMsr(core string) error {
	path := fmt.Sprintf(msrPartialPath, core)
	err := checkFile(path)
	if err != nil {
		return err
	}
	msrFile, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("error opening MSR file on path %q: %w", path, err)
	}
	defer msrFile.Close()

	err = m.readDataFromMsr(core, msrFile)
	if err != nil {
		return fmt.Errorf("error reading data from MSR for core %q: %w", core, err)
	}
	return nil
}

func (m *msrServiceImpl) readSingleMsr(core string, msr string) (uint64, error) {
	path := fmt.Sprintf(msrPartialPath, core)
	err := checkFile(path)
	if err != nil {
		return 0, err
	}
	msrFile, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("error opening MSR file on path %q: %w", path, err)
	}
	defer msrFile.Close()

	var msrAddress int64
	switch msr {
	case msrTurboRatioLimitString:
		msrAddress = turboRatioLimitLocation
	case msrTurboRatioLimit1String:
		msrAddress = turboRatioLimit1Location
	case msrTurboRatioLimit2String:
		msrAddress = turboRatioLimit2Location
	case msrAtomCoreTurboRatiosString:
		msrAddress = atomCoreTurboRatiosLocation
	case msrUncorePerfStatusString:
		msrAddress = uncorePerfStatusLocation
	case msrPlatformInfoString:
		msrAddress = platformInfo
	case msrFSBFreqString:
		msrAddress = fsbFreq
	default:
		return 0, fmt.Errorf("incorect name of MSR %s", msr)
	}

	value, err := m.fs.readFileAtOffsetToUint64(msrFile, msrAddress)
	if err != nil {
		return 0, err
	}

	return value, nil
}

func (m *msrServiceImpl) readDataFromMsr(core string, reader io.ReaderAt) error {
	g, ctx := errgroup.WithContext(context.Background())

	// Create and populate a map that contains msr offsets along with their respective channels
	msrOffsetsWithChannels := make(map[int64]chan uint64)
	for _, offset := range m.msrOffsets {
		msrOffsetsWithChannels[offset] = make(chan uint64)
	}

	// Start a goroutine for each msr offset
	for offset, channel := range msrOffsetsWithChannels {
		// Wrap around function to avoid race on loop counter
		func(off int64, ch chan uint64) {
			g.Go(func() error {
				defer close(ch)

				err := m.readValueFromFileAtOffset(ctx, ch, reader, off)
				if err != nil {
					return fmt.Errorf("error reading MSR file: %w", err)
				}

				return nil
			})
		}(offset, channel)
	}

	newC3 := <-msrOffsetsWithChannels[c3StateResidencyLocation]
	newC6 := <-msrOffsetsWithChannels[c6StateResidencyLocation]
	newC7 := <-msrOffsetsWithChannels[c7StateResidencyLocation]
	newMperf := <-msrOffsetsWithChannels[maximumFrequencyClockCountLocation]
	newAperf := <-msrOffsetsWithChannels[actualFrequencyClockCountLocation]
	newTsc := <-msrOffsetsWithChannels[timestampCounterLocation]
	newThrottleTemp := <-msrOffsetsWithChannels[throttleTemperatureLocation]
	newTemp := <-msrOffsetsWithChannels[temperatureLocation]

	if err := g.Wait(); err != nil {
		return fmt.Errorf("received error during reading MSR values in goroutines: %w", err)
	}

	m.cpuCoresData[core].c3Delta = newC3 - m.cpuCoresData[core].c3
	m.cpuCoresData[core].c6Delta = newC6 - m.cpuCoresData[core].c6
	m.cpuCoresData[core].c7Delta = newC7 - m.cpuCoresData[core].c7
	m.cpuCoresData[core].mperfDelta = newMperf - m.cpuCoresData[core].mperf
	m.cpuCoresData[core].aperfDelta = newAperf - m.cpuCoresData[core].aperf
	m.cpuCoresData[core].timeStampCounterDelta = newTsc - m.cpuCoresData[core].timeStampCounter

	m.cpuCoresData[core].c3 = newC3
	m.cpuCoresData[core].c6 = newC6
	m.cpuCoresData[core].c7 = newC7
	m.cpuCoresData[core].mperf = newMperf
	m.cpuCoresData[core].aperf = newAperf
	m.cpuCoresData[core].timeStampCounter = newTsc
	// MSR (1A2h) IA32_TEMPERATURE_TARGET bits 23:16.
	m.cpuCoresData[core].throttleTemp = int64((newThrottleTemp >> 16) & 0xFF)
	// MSR (19Ch) IA32_THERM_STATUS bits 22:16.
	m.cpuCoresData[core].temp = int64((newTemp >> 16) & 0x7F)

	return nil
}

func (m *msrServiceImpl) readValueFromFileAtOffset(ctx context.Context, ch chan uint64, reader io.ReaderAt, offset int64) error {
	value, err := m.fs.readFileAtOffsetToUint64(reader, offset)
	if err != nil {
		return err
	}

	// Detect context cancellation and return an error if other goroutine fails
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- value:
	}

	return nil
}

// setCPUCores initialize cpuCoresData map.
func (m *msrServiceImpl) setCPUCores() error {
	m.cpuCoresData = make(map[string]*msrData)
	cpuPrefix := "cpu"
	cpuCore := fmt.Sprintf("%s%s", cpuPrefix, "[0-9]*")
	cpuCorePattern := fmt.Sprintf("%s/%s", systemCPUPath, cpuCore)
	cpuPaths, err := m.fs.getStringsMatchingPatternOnPath(cpuCorePattern)
	if err != nil {
		return err
	}
	if len(cpuPaths) == 0 {
		m.log.Debugf("CPU core data wasn't found using pattern: %s", cpuCorePattern)
		return nil
	}

	for _, cpuPath := range cpuPaths {
		core := strings.TrimPrefix(filepath.Base(cpuPath), cpuPrefix)
		if m.cpuCores != nil {
			coreInt, _ := strconv.Atoi(core)
			if !Contains(m.cpuCores, coreInt) {
				continue
			}
		}
		m.cpuCoresData[core] = &msrData{
			mperf:                 0,
			aperf:                 0,
			timeStampCounter:      0,
			c3:                    0,
			c6:                    0,
			c7:                    0,
			throttleTemp:          0,
			temp:                  0,
			mperfDelta:            0,
			aperfDelta:            0,
			timeStampCounterDelta: 0,
			c3Delta:               0,
			c6Delta:               0,
			c7Delta:               0,
		}
	}

	return nil
}

func newMsrServiceWithFs(logger telegraf.Logger, fs fileService, cores []string) *msrServiceImpl {
	parsedCores := parseCores(logger, cores)
	msrService := &msrServiceImpl{
		fs:       fs,
		log:      logger,
		cpuCores: parsedCores,
	}
	err := msrService.setCPUCores()
	if err != nil {
		// This error does not prevent plugin from working thus it is not returned.
		msrService.log.Error(err)
	}

	msrService.msrOffsets = []int64{c3StateResidencyLocation, c6StateResidencyLocation, c7StateResidencyLocation,
		maximumFrequencyClockCountLocation, actualFrequencyClockCountLocation, timestampCounterLocation,
		throttleTemperatureLocation, temperatureLocation}
	return msrService
}

func parseCores(logger telegraf.Logger, cores []string) []int {
	if cores == nil {
		logger.Debug("all possible cores will be configured")
		return nil
	}
	if len(cores) == 0 {
		logger.Warn("an empty list of cores was provided. All possible cores will be configured")
		return nil
	}
	result, err := parseIntRanges(logger, cores)
	if err != nil {
		logger.Warnf("Error while parsing list of cores: %w. All possible cores will be configured", err)
		return nil
	}
	return result
}

func parseIntRanges(logger telegraf.Logger, ranges []string) ([]int, error) {
	var ids []int
	var duplicatedIDs []int
	var err error
	ids, err = parseIDs(ranges)
	if err != nil {
		return nil, err
	}
	ids, duplicatedIDs = removeDuplicateValues(ids)
	for _, duplication := range duplicatedIDs {
		logger.Warn("duplicated id number `%d` will be removed", duplication)
	}
	return ids, nil
}

func parseIDs(allIDsStrings []string) ([]int, error) {
	var result []int
	for _, idsString := range allIDsStrings {
		ids := strings.Split(idsString, ",")

		for _, id := range ids {
			id := strings.TrimSpace(id)
			// a-b support
			var start, end uint
			n, err := fmt.Sscanf(id, "%d-%d", &start, &end)
			if err == nil && n == 2 {
				if start >= end {
					return nil, fmt.Errorf("`%d` is equal or greater than `%d`", start, end)
				}
				for ; start <= end; start++ {
					if len(result)+1 > maxIDsSize {
						return nil, fmt.Errorf("requested number of IDs exceeds max size `%d`", maxIDsSize)
					}
					result = append(result, int(start))
				}
				continue
			}
			// Single value
			num, err := strconv.Atoi(id)
			if err != nil {
				return nil, fmt.Errorf("wrong format for id number %q: %w", id, err)
			}
			if len(result)+1 > maxIDsSize {
				return nil, fmt.Errorf("requested number of IDs exceeds max size `%d`", maxIDsSize)
			}
			result = append(result, num)
		}
	}
	return result, nil
}

func removeDuplicateValues(intSlice []int) (result []int, duplicates []int) {
	keys := make(map[int]bool)

	for _, entry := range intSlice {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			result = append(result, entry)
		} else {
			duplicates = append(duplicates, entry)
		}
	}
	return result, duplicates
}

func contains[T comparable](s []T, e T) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}
