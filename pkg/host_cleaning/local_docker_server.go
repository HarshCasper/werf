package host_cleaning

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"

	"github.com/dustin/go-humanize"

	"github.com/werf/lockgate"
	"github.com/werf/logboek"
	"github.com/werf/werf/pkg/container_runtime"
	"github.com/werf/werf/pkg/docker"
	"github.com/werf/werf/pkg/image"
	"github.com/werf/werf/pkg/storage/lrumeta"
	"github.com/werf/werf/pkg/volumeutils"
	"github.com/werf/werf/pkg/werf"

	"github.com/werf/kubedog/pkg/utils"
)

const (
	MinImagesToDelete = 10
)

func GetLocalDockerServerStoragePath(ctx context.Context) (string, error) {
	dockerInfo, err := docker.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("unable to get docker info: %s", err)
	}

	var storagePath string

	if dockerInfo.OperatingSystem == "Docker Desktop" {
		switch runtime.GOOS {
		case "windows":
			storagePath = filepath.Join(os.Getenv("HOMEDRIVE"), `\\ProgramData\DockerDesktop\vm-data\`)

		case "darwin":
			storagePath = filepath.Join(os.Getenv("HOME"), "Library/Containers/com.docker.docker/Data")
		}
	} else {
		storagePath = dockerInfo.DockerRootDir
	}

	if _, err := os.Stat(storagePath); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("error accessing %q: %s", storagePath, err)
	}
	return storagePath, nil
}

func getDockerServerStoragePath(ctx context.Context, dockerServerStoragePathOption string) (string, error) {
	var dockerServerStoragePath string
	if dockerServerStoragePathOption != "" {
		dockerServerStoragePath = dockerServerStoragePathOption
	} else {
		path, err := GetLocalDockerServerStoragePath(ctx)
		if err != nil {
			return "", err
		}
		dockerServerStoragePath = path
	}

	return dockerServerStoragePath, nil
}

func ShouldRunAutoGCForLocalDockerServer(ctx context.Context, allowedVolumeUsagePercentage float64, dockerServerStoragePath string) (bool, error) {
	if dockerServerStoragePath == "" {
		return false, nil
	}

	vu, err := volumeutils.GetVolumeUsageByPath(ctx, dockerServerStoragePath)
	if err != nil {
		return false, fmt.Errorf("error getting volume usage by path %q: %s", dockerServerStoragePath, err)
	}

	return vu.Percentage > allowedVolumeUsagePercentage, nil
}

type LocalDockerServerStorageCheckResult struct {
	VolumeUsage      volumeutils.VolumeUsage
	TotalImagesBytes uint64
	ImagesDescs      []*LocalImageDesc
}

func (checkResult *LocalDockerServerStorageCheckResult) GetBytesToFree(targetVolumeUsage float64) uint64 {
	allowedVolumeUsageToFree := checkResult.VolumeUsage.Percentage - targetVolumeUsage
	bytesToFree := uint64((float64(checkResult.VolumeUsage.TotalBytes) / 100.0) * allowedVolumeUsageToFree)
	return bytesToFree
}

func GetLocalDockerServerStorageCheck(ctx context.Context, dockerServerStoragePath string) (*LocalDockerServerStorageCheckResult, error) {
	res := &LocalDockerServerStorageCheckResult{}

	vu, err := volumeutils.GetVolumeUsageByPath(ctx, dockerServerStoragePath)
	if err != nil {
		return nil, fmt.Errorf("error getting volume usage by path %q: %s", dockerServerStoragePath, err)
	}
	res.VolumeUsage = vu

	filterSet := filters.NewArgs()
	filterSet.Add("label", image.WerfLabel)
	filterSet.Add("label", image.WerfStageDigestLabel)
	images, err := docker.Images(ctx, types.ImageListOptions{Filters: filterSet})
	if err != nil {
		return nil, fmt.Errorf("unable to get docker images: %s", err)
	}

	for _, imageSummary := range images {
		data, _ := json.Marshal(imageSummary)
		logboek.Context(ctx).Debug().LogF("Image summary:\n%s\n---\n", data)

		res.TotalImagesBytes += uint64(imageSummary.Size)

		lastUsedAt := time.Unix(imageSummary.Created, 0)

		for _, ref := range imageSummary.RepoTags {
			if ref == "<none>:<none>" {
				continue
			}

			lastRecentlyUsedAt, err := lrumeta.CommonLRUImagesCache.GetImageLastAccessTime(ctx, ref)
			if err != nil {
				return nil, fmt.Errorf("error accessing last recently used images cache: %s", err)
			}

			if lastRecentlyUsedAt.IsZero() {
				continue
			}

			lastUsedAt = lastRecentlyUsedAt
		}

		desc := &LocalImageDesc{
			ImageSummary: imageSummary,
			LastUsedAt:   lastUsedAt,
		}
		res.ImagesDescs = append(res.ImagesDescs, desc)
	}

	sort.Sort(ImagesLruSort(res.ImagesDescs))

	return res, nil
}

func RunGCForLocalDockerServer(ctx context.Context, allowedVolumeUsagePercentage, allowedVolumeUsageMarginPercentage float64, dockerServerStoragePath string, force, dryRun bool) error {
	if dockerServerStoragePath == "" {
		return nil
	}

	targetVolumeUsage := allowedVolumeUsagePercentage - allowedVolumeUsageMarginPercentage

	checkResult, err := GetLocalDockerServerStorageCheck(ctx, dockerServerStoragePath)
	if err != nil {
		return fmt.Errorf("error getting local docker server storage check: %s", err)
	}

	bytesToFree := checkResult.GetBytesToFree(targetVolumeUsage)

	if checkResult.VolumeUsage.Percentage <= allowedVolumeUsagePercentage {
		logboek.Context(ctx).Default().LogBlock("Local docker server storage check").Do(func() {
			logboek.Context(ctx).Default().LogF("Docker server storage path: %s\n", dockerServerStoragePath)
			logboek.Context(ctx).Default().LogF("Volume usage: %s / %s\n", humanize.Bytes(checkResult.VolumeUsage.UsedBytes), humanize.Bytes(checkResult.VolumeUsage.TotalBytes))
			logboek.Context(ctx).Default().LogF("Allowed volume usage percentage: %s <= %s — %s\n", utils.GreenF("%0.2f%%", checkResult.VolumeUsage.Percentage), utils.BlueF("%0.2f%%", allowedVolumeUsagePercentage), utils.GreenF("OK"))
		})

		return nil
	}

	logboek.Context(ctx).Default().LogBlock("Local docker server storage check").Do(func() {
		logboek.Context(ctx).Default().LogF("Docker server storage path: %s\n", dockerServerStoragePath)
		logboek.Context(ctx).Default().LogF("Volume usage: %s / %s\n", humanize.Bytes(checkResult.VolumeUsage.UsedBytes), humanize.Bytes(checkResult.VolumeUsage.TotalBytes))
		logboek.Context(ctx).Default().LogF("Allowed percentage level exceeded: %s > %s — %s\n", utils.RedF("%0.2f%%", checkResult.VolumeUsage.Percentage), utils.YellowF("%0.2f%%", allowedVolumeUsagePercentage), utils.RedF("HIGH VOLUME USAGE"))
		logboek.Context(ctx).Default().LogF("Target percentage level after cleanup: %0.2f%% - %0.2f%% (margin) = %s\n", allowedVolumeUsagePercentage, allowedVolumeUsageMarginPercentage, utils.BlueF("%0.2f%%", targetVolumeUsage))
		logboek.Context(ctx).Default().LogF("Needed to free: %s\n", utils.RedF("%s", humanize.Bytes(bytesToFree)))
		logboek.Context(ctx).Default().LogF("Available images to free: %s\n", utils.YellowF("%d (~ %s)", len(checkResult.ImagesDescs), humanize.Bytes(checkResult.TotalImagesBytes)))
	})

	for {
		var freedBytes uint64
		var freedImagesCount uint64
		var acquiredHostLocks []lockgate.LockHandle

		if len(checkResult.ImagesDescs) > 0 {
			if err := logboek.Context(ctx).Default().LogProcess("Running cleanup for least recently used docker images created by werf").DoError(func() error {
			DeleteImages:
				for _, desc := range checkResult.ImagesDescs {
					imageRemovalFailed := false

					for _, ref := range desc.ImageSummary.RepoTags {
						var args []string

						if ref == "<none>:<none>" {
							args = append(args, desc.ImageSummary.ID)
						} else {
							lockName := container_runtime.ImageLockName(ref)

							isLocked, lock, err := werf.AcquireHostLock(ctx, lockName, lockgate.AcquireOptions{NonBlocking: true})
							if err != nil {
								return fmt.Errorf("error locking image %q: %s", lockName, err)
							}

							if !isLocked {
								logboek.Context(ctx).Default().LogFDetails("Image %q is locked at the moment: skip removal\n", ref)
								continue DeleteImages
							}

							acquiredHostLocks = append(acquiredHostLocks, lock)

							args = append(args, ref)
						}

						if force {
							args = append(args, "--force")
						}

						logboek.Context(ctx).Default().LogF("Removing %s\n", ref)
						if dryRun {
							continue
						}

						if err := docker.CliRmi(ctx, args...); err != nil {
							logboek.Context(ctx).Warn().LogF("failed to remove local docker image %q: %s\n", ref, err)
							imageRemovalFailed = true
						}
					}

					if !imageRemovalFailed {
						freedBytes += uint64(desc.ImageSummary.Size)
						freedImagesCount++
					}

					if freedImagesCount < MinImagesToDelete {
						continue
					}

					if freedBytes > bytesToFree {
						break
					}
				}

				logboek.Context(ctx).Default().LogF("Freed images: %s\n", utils.GreenF("%d (~ %s)", freedImagesCount, humanize.Bytes(freedBytes)))

				return nil
			}); err != nil {
				return err
			}
		}

		if freedImagesCount == 0 {
			logboek.Context(ctx).Warn().LogF("WARNING: Detected high docker storage volume usage, while no werf images available to cleanup!\n")
			logboek.Context(ctx).Warn().LogF("WARNING:\n")
			logboek.Context(ctx).Warn().LogF("WARNING: Werf tries to maintain host clean by deleting:\n")
			logboek.Context(ctx).Warn().LogF("WARNING:  - old unused files from werf caches (which are stored in the ~/.werf/local_cache);\n")
			logboek.Context(ctx).Warn().LogF("WARNING:  - old temporary service files /tmp/werf-project-data-* and /tmp/werf-config-render-*;\n")
			logboek.Context(ctx).Warn().LogF("WARNING:  - least recently used werf images (only >= v1.2 werf images could be removed, note that werf <= v1.1 images will not be deleted by this cleanup);\n")
			logboek.Context(ctx).Warn().LogOptionalLn()
		}

		for _, lock := range acquiredHostLocks {
			if err := werf.ReleaseHostLock(lock); err != nil {
				return fmt.Errorf("unable to release lock %q: %s", lock.LockName, err)
			}
		}

		commonOptions := CommonOptions{
			RmContainersThatUseWerfImages: force,
			SkipUsedImages:                !force,
			RmiForce:                      force,
			RmForce:                       true,
			DryRun:                        dryRun,
		}

		if err := logboek.Context(ctx).Default().LogProcess("Running cleanup for docker containers created by werf").DoError(func() error {
			return safeContainersCleanup(ctx, commonOptions)
		}); err != nil {
			return err
		}

		if err := logboek.Context(ctx).Default().LogProcess("Running cleanup for dangling docker images created by werf").DoError(func() error {
			return safeDanglingImagesCleanup(ctx, commonOptions)
		}); err != nil {
			return err
		}

		if freedImagesCount == 0 {
			break
		}
		if dryRun {
			break
		}

		logboek.Context(ctx).Default().LogOptionalLn()

		checkResult, err = GetLocalDockerServerStorageCheck(ctx, dockerServerStoragePath)
		if err != nil {
			return fmt.Errorf("error getting local docker server storage check: %s", err)
		}

		if checkResult.VolumeUsage.Percentage <= targetVolumeUsage {
			logboek.Context(ctx).Default().LogBlock("Local docker server storage check").Do(func() {
				logboek.Context(ctx).Default().LogF("Docker server storage path: %s\n", dockerServerStoragePath)
				logboek.Context(ctx).Default().LogF("Volume usage: %s / %s\n", humanize.Bytes(checkResult.VolumeUsage.UsedBytes), humanize.Bytes(checkResult.VolumeUsage.TotalBytes))
				logboek.Context(ctx).Default().LogF("Target volume usage percentage: %s <= %s — %s\n", utils.GreenF("%0.2f%%", checkResult.VolumeUsage.Percentage), utils.BlueF("%0.2f%%", targetVolumeUsage), utils.GreenF("OK"))
			})

			break
		}

		bytesToFree = checkResult.GetBytesToFree(targetVolumeUsage)

		logboek.Context(ctx).Default().LogBlock("Local docker server storage check").Do(func() {
			logboek.Context(ctx).Default().LogF("Docker server storage path: %s\n", dockerServerStoragePath)
			logboek.Context(ctx).Default().LogF("Volume usage: %s / %s\n", humanize.Bytes(checkResult.VolumeUsage.UsedBytes), humanize.Bytes(checkResult.VolumeUsage.TotalBytes))
			logboek.Context(ctx).Default().LogF("Target volume usage percentage: %s > %s — %s\n", utils.RedF("%0.2f%%", checkResult.VolumeUsage.Percentage), utils.BlueF("%0.2f%%", targetVolumeUsage), utils.RedF("HIGH VOLUME USAGE"))
			logboek.Context(ctx).Default().LogF("Needed to free: %s\n", utils.RedF("%s", humanize.Bytes(bytesToFree)))
			logboek.Context(ctx).Default().LogF("Available images to free: %s\n", utils.YellowF("%d (~ %s)", len(checkResult.ImagesDescs), humanize.Bytes(checkResult.TotalImagesBytes)))
		})
	}

	return nil
}

type LocalImageDesc struct {
	ImageSummary types.ImageSummary
	LastUsedAt   time.Time
}

type ImagesLruSort []*LocalImageDesc

func (a ImagesLruSort) Len() int { return len(a) }
func (a ImagesLruSort) Less(i, j int) bool {
	return a[i].LastUsedAt.Before(a[j].LastUsedAt)
}
func (a ImagesLruSort) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

func safeDanglingImagesCleanup(ctx context.Context, options CommonOptions) error {
	images, err := werfImagesByFilterSet(ctx, danglingFilterSet())
	if err != nil {
		return err
	}

	var imagesToRemove []types.ImageSummary

	for _, img := range images {
		imagesToRemove = append(imagesToRemove, img)
	}

	imagesToRemove, err = processUsedImages(ctx, imagesToRemove, options)
	if err != nil {
		return err
	}

	if err := imagesRemove(ctx, imagesToRemove, options); err != nil {
		return err
	}

	return nil
}

func safeContainersCleanup(ctx context.Context, options CommonOptions) error {
	containers, err := werfContainersByFilterSet(ctx, filters.NewArgs())
	if err != nil {
		return fmt.Errorf("cannot get stages build containers: %s", err)
	}

	for _, container := range containers {
		var containerName string
		for _, name := range container.Names {
			if strings.HasPrefix(name, fmt.Sprintf("/%s", image.StageContainerNamePrefix)) {
				containerName = strings.TrimPrefix(name, "/")
				break
			}
		}

		if containerName == "" {
			logboek.Context(ctx).Warn().LogF("Ignore bad container %s\n", container.ID)
			continue
		}

		if err := func() error {
			containerLockName := container_runtime.ContainerLockName(containerName)
			isLocked, lock, err := werf.AcquireHostLock(ctx, containerLockName, lockgate.AcquireOptions{NonBlocking: true})
			if err != nil {
				return fmt.Errorf("failed to lock %s for container %s: %s", containerLockName, logContainerName(container), err)
			}

			if !isLocked {
				logboek.Context(ctx).Default().LogFDetails("Ignore container %s used by another process\n", logContainerName(container))
				return nil
			}
			defer werf.ReleaseHostLock(lock)

			if err := containersRemove(ctx, []types.Container{container}, options); err != nil {
				return fmt.Errorf("failed to remove container %s: %s", logContainerName(container), err)
			}

			return nil
		}(); err != nil {
			return err
		}
	}

	return nil
}
