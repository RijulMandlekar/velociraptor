/*
   Velociraptor - Hunting Evil
   Copyright (C) 2019 Velocidex Innovations.

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published
   by the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/
// Manage in memory hunt replication.  For performance, the hunts
// table is mirrored in memory and refreshed periodically. The clients
// are then compared against it on each poll and hunts are dispatched
// as needed.
package flows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path"
	"sort"
	"time"

	"github.com/Velocidex/ordereddict"
	"github.com/golang/protobuf/proto"
	errors "github.com/pkg/errors"
	api_proto "www.velocidex.com/golang/velociraptor/api/proto"
	config_proto "www.velocidex.com/golang/velociraptor/config/proto"
	"www.velocidex.com/golang/velociraptor/constants"
	"www.velocidex.com/golang/velociraptor/datastore"
	"www.velocidex.com/golang/velociraptor/paths"
	"www.velocidex.com/golang/velociraptor/services"
	vql_subsystem "www.velocidex.com/golang/velociraptor/vql"
)

func GetNewHuntId() string {
	result := make([]byte, 8)
	buf := make([]byte, 4)

	_, _ = rand.Read(buf)
	hex.Encode(result, buf)

	return constants.HUNT_PREFIX + string(result)
}

// Backwards compatibility: Figure out the list of collected hunts
// from the hunt object's request
func FindCollectedArtifacts(
	config_obj *config_proto.Config,
	hunt *api_proto.Hunt) {
	if hunt == nil || hunt.StartRequest == nil ||
		hunt.StartRequest.Artifacts == nil {
		return
	}

	// Hunt already has artifacts list.
	if len(hunt.Artifacts) > 0 {
		return
	}

	hunt.Artifacts = hunt.StartRequest.Artifacts
	hunt.ArtifactSources = []string{}
	for _, artifact := range hunt.StartRequest.Artifacts {
		for _, source := range GetArtifactSources(
			config_obj, artifact) {
			hunt.ArtifactSources = append(
				hunt.ArtifactSources,
				path.Join(artifact, source))
		}
	}
}

func GetArtifactSources(
	config_obj *config_proto.Config,
	artifact string) []string {
	result := []string{}
	manager, err := services.GetRepositoryManager()
	if err != nil {
		return nil
	}

	repository, err := manager.GetGlobalRepository(config_obj)
	if err == nil {
		artifact_obj, pres := repository.Get(config_obj, artifact)
		if pres {
			for _, source := range artifact_obj.Sources {
				result = append(result, source.Name)
			}
		}
	}
	return result
}

func CreateHunt(
	ctx context.Context,
	config_obj *config_proto.Config,
	acl_manager vql_subsystem.ACLManager,
	hunt *api_proto.Hunt) (string, error) {
	db, err := datastore.GetDB(config_obj)
	if err != nil {
		return "", err
	}

	if hunt.Stats == nil {
		hunt.Stats = &api_proto.HuntStats{}
	}

	if hunt.HuntId == "" {
		hunt.HuntId = GetNewHuntId()
	}

	if hunt.StartRequest == nil || hunt.StartRequest.Artifacts == nil {
		return "", errors.New("No artifacts to collect.")
	}

	hunt.CreateTime = uint64(time.Now().UTC().UnixNano() / 1000)
	if hunt.Expires == 0 {
		hunt.Expires = uint64(time.Now().Add(7*24*time.Hour).
			UTC().UnixNano() / 1000)
	}

	if hunt.Expires < hunt.CreateTime {
		return "", errors.New("Hunt expiry is in the past!")
	}

	// Set the artifacts information in the hunt object itself.
	hunt.Artifacts = hunt.StartRequest.Artifacts
	hunt.ArtifactSources = []string{}
	for _, artifact := range hunt.StartRequest.Artifacts {
		for _, source := range GetArtifactSources(
			config_obj, artifact) {
			hunt.ArtifactSources = append(
				hunt.ArtifactSources, path.Join(artifact, source))
		}
	}

	manager, err := services.GetRepositoryManager()
	if err != nil {
		return "", err
	}

	repository, err := manager.GetGlobalRepository(config_obj)
	if err != nil {
		return "", err
	}

	// Compile the start request and store it in the hunt. We will
	// use this compiled version to launch all other flows from
	// this hunt rather than re-compile the artifact each
	// time. This ensures that if the artifact definition is
	// changed after this point, the hunt will continue to
	// schedule consistent VQL on the clients.
	launcher, err := services.GetLauncher()
	if err != nil {
		return "", err
	}

	compiled, err := launcher.CompileCollectorArgs(
		ctx, config_obj, acl_manager, repository,
		true, /* should_obfuscate */
		hunt.StartRequest)
	if err != nil {
		return "", err
	}
	hunt.StartRequest.CompiledCollectorArgs = append(
		hunt.StartRequest.CompiledCollectorArgs, compiled...)

	// We allow our caller to determine if hunts are created in
	// the running state or the paused state.
	if hunt.State == api_proto.Hunt_UNSET {
		hunt.State = api_proto.Hunt_PAUSED

		// IF we are creating the hunt in the running state
		// set it started.
	} else if hunt.State == api_proto.Hunt_RUNNING {
		hunt.StartTime = hunt.CreateTime

		// Notify all the clients.
		notifier := services.GetNotifier()
		if notifier != nil {
			err = notifier.NotifyByRegex(config_obj, "^[Cc]\\.")
			if err != nil {
				return "", err
			}
		}
	}

	hunt_path_manager := paths.NewHuntPathManager(hunt.HuntId)
	err = db.SetSubject(config_obj, hunt_path_manager.Path(), hunt)
	if err != nil {
		return "", err
	}

	// Trigger a refresh of the hunt dispatcher. This guarantees
	// that fresh data will be read in subsequent ListHunt()
	// calls.
	err = services.GetHuntDispatcher().Refresh(config_obj)

	return hunt.HuntId, err
}

func ListHunts(config_obj *config_proto.Config, in *api_proto.ListHuntsRequest) (
	*api_proto.ListHuntsResponse, error) {

	result := &api_proto.ListHuntsResponse{}

	dispatcher := services.GetHuntDispatcher()
	if dispatcher == nil {
		return nil, errors.New("Hunt dispatcher not initialized")
	}

	err := dispatcher.ApplyFuncOnHunts(
		func(hunt *api_proto.Hunt) error {
			if uint64(len(result.Items)) < in.Offset {
				return nil
			}

			if uint64(len(result.Items)) >= in.Offset+in.Count {
				return constants.STOP_ITERATION
			}

			if in.IncludeArchived || hunt.State != api_proto.Hunt_ARCHIVED {
				result.Items = append(result.Items, hunt)
			}
			return nil
		})

	if err == constants.STOP_ITERATION {
		err = nil
	}

	sort.Slice(result.Items, func(i, j int) bool {
		return result.Items[i].CreateTime > result.Items[j].CreateTime
	})

	return result, err
}

func GetHunt(config_obj *config_proto.Config, in *api_proto.GetHuntRequest) (
	hunt *api_proto.Hunt, err error) {

	var result *api_proto.Hunt

	dispatcher := services.GetHuntDispatcher()
	if dispatcher == nil {
		return nil, errors.New("Hunt dispatcher not valid")
	}

	err = dispatcher.ModifyHunt(in.HuntId,
		func(hunt_obj *api_proto.Hunt) error {
			// Make a copy of the hunt
			result = proto.Clone(hunt_obj).(*api_proto.Hunt)

			// We do not modify the hunt so it is not dirty.
			return notModified
		})
	if err != notModified {
		return nil, err
	}

	FindCollectedArtifacts(config_obj, result)

	if result == nil || result.Stats == nil {
		return result, errors.New("Not found")
	}

	result.Stats.AvailableDownloads, _ = availableHuntDownloadFiles(config_obj, in.HuntId)

	return result, nil
}

// availableHuntDownloadFiles returns the prepared zip downloads available to
// be fetched by the user at this moment.
func availableHuntDownloadFiles(config_obj *config_proto.Config,
	hunt_id string) (*api_proto.AvailableDownloads, error) {

	hunt_path_manager := paths.NewHuntPathManager(hunt_id)
	download_file := hunt_path_manager.GetHuntDownloadsFile(false, "")
	download_path := path.Dir(download_file)

	return getAvailableDownloadFiles(config_obj, download_path)
}

// This method modifies the hunt. Only the following modifications are allowed:

// 1. A hunt in the paused state can go to the running state. This
//    will update the StartTime.
// 2. A hunt in the running state can go to the Stop state
// 3. A hunt's description can be modified.
func ModifyHunt(
	ctx context.Context,
	config_obj *config_proto.Config,
	hunt_modification *api_proto.Hunt,
	user string) error {
	dispatcher := services.GetHuntDispatcher()
	err := dispatcher.ModifyHunt(
		hunt_modification.HuntId,
		func(hunt *api_proto.Hunt) error {
			if hunt.Stats == nil {
				return errors.New("Invalid hunt")
			}

			// Is the description changed?
			if hunt_modification.HuntDescription != "" {
				hunt.HuntDescription = hunt_modification.HuntDescription

				// Archive the hunt.
			} else if hunt_modification.State == api_proto.Hunt_ARCHIVED {
				hunt.State = api_proto.Hunt_ARCHIVED

				row := ordereddict.NewDict().
					Set("Timestamp", time.Now().UTC().Unix()).
					Set("Hunt", hunt).
					Set("User", user)

				journal, err := services.GetJournal()
				if err != nil {
					return err
				}

				err = journal.PushRowsToArtifact(config_obj,
					[]*ordereddict.Dict{row}, "System.Hunt.Archive",
					"server", hunt_modification.HuntId)
				if err != nil {
					return err
				}

				// We are trying to start the hunt.
			} else if hunt_modification.State == api_proto.Hunt_RUNNING {

				// The hunt has been expired.
				if hunt.Stats.Stopped {
					return errors.New("Can not start a stopped hunt.")
				}

				hunt.State = api_proto.Hunt_RUNNING
				hunt.StartTime = uint64(time.Now().UnixNano() / 1000)

				// We are trying to pause or stop the hunt.
			} else {
				hunt.State = api_proto.Hunt_STOPPED
			}

			// Returning nil indicates to the hunt manager
			// that the hunt was successfully modified. It
			// will then flush it to disk.
			return nil
		})

	if err != nil {
		return err
	}

	// Notify all the clients about the new hunt. New hunts are
	// not that common so notifying all the clients at once is
	// probably ok.
	notifier := services.GetNotifier()
	if notifier == nil {
		return errors.New("Notifier not ready")
	}
	return notifier.NotifyByRegex(config_obj, "^[Cc]\\.")
}
