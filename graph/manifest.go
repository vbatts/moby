package graph

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"path"

	"github.com/docker/docker/engine"
	"github.com/docker/docker/pkg/tarsum"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/runconfig"
)

func (s *TagStore) CmdManifest(job *engine.Job) engine.Status {
	if len(job.Args) != 1 {
		return job.Errorf("usage: %s NAME", job.Name)
	}
	name := job.Args[0]
	tag := job.Getenv("tag")
	if tag == "" {
		tag = "latest"
	}

	// Resolve the Repository name from fqn to endpoint + name
	_, remoteName, err := registry.ResolveRepositoryName(name)
	if err != nil {
		return job.Error(err)
	}

	manifest := &registry.ManifestData{
		Name: remoteName,
		Tag:  tag,
	}
	localRepo, exists := s.Repositories[name]
	if !exists {
		return job.Errorf("Repo does not exist: %s", name)
	}

	layerId, exists := localRepo[tag]
	if !exists {
		return job.Errorf("Tag does not exist for %s: %s", name, tag)
	}
	tarsums := make([]string, 0, 4)
	layersSeen := make(map[string]bool)

	layer, err := s.graph.Get(layerId)
	manifest.Architecture = layer.Architecture
	var metadata runconfig.Config
	metadata = *layer.Config
	history := make(map[string]string)

	for ; layer != nil; layer, err = layer.GetParent() {
		if err != nil {
			return job.Error(err)
		}

		if layersSeen[layer.ID] {
			break
		}
		if layer.Config != nil && metadata.Image != layer.ID {
			err = runconfig.Merge(&metadata, layer.Config)
			if err != nil {
				return job.Error(err)
			}
		}
		archive, err := layer.TarLayer()
		if err != nil {
			return job.Error(err)
		}

		tarSum := &tarsum.TarSum{Reader: archive, DisableCompression: true}
		if _, err := io.Copy(ioutil.Discard, tarSum); err != nil {
			return job.Error(err)
		}

		layersSeen[layer.ID] = true
		tarId := tarSum.Sum(nil)
		tarsums = append(tarsums, tarId)

		jsonData, err := ioutil.ReadFile(path.Join(s.graph.Root, layer.ID, "json"))
		if err != nil {
			return job.Error(fmt.Errorf("Cannot retrieve the path for {%s}: %s", layer.ID, err))
		}
		history[tarId] = string(jsonData)
	}

	manifest.BlobSums = tarsums
	manifest.History = history

	manifestBytes, err := json.MarshalIndent(manifest, "", "   ")
	if err != nil {
		return job.Error(err)
	}

	_, err = job.Stdout.Write(manifestBytes)
	if err != nil {
		return job.Error(err)
	}

	return engine.StatusOK
}
