/*Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package skyring

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/skyrings/skyring-common/conf"
	"github.com/skyrings/skyring-common/db"
	"github.com/skyrings/skyring-common/models"
	"github.com/skyrings/skyring-common/monitoring"
	"github.com/skyrings/skyring-common/tools/logger"
	"github.com/skyrings/skyring-common/tools/schedule"
	"github.com/skyrings/skyring-common/tools/uuid"
	"gopkg.in/mgo.v2/bson"
	"net/http"
	"strings"
	"sync"
	"time"
)

func GetSLU(cluster_id *uuid.UUID, slu_id uuid.UUID) (slu models.StorageLogicalUnit, err error) {
	if cluster_id == nil {
		return slu, fmt.Errorf("Cluster Id not available for slu with id %v", slu_id)
	}
	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()
	coll := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_STORAGE_LOGICAL_UNITS)
	if err := coll.Find(bson.M{"clusterid": *cluster_id, "sluid": slu_id}).One(&slu); err != nil {
		return slu, fmt.Errorf("Error getting the slu: %v for cluster: %v. error: %v", slu_id, *cluster_id, err)
	}
	if slu.Name == "" {
		return slu, fmt.Errorf("Slu: %v not found for cluster: %v", slu_id, *cluster_id)
	}
	return slu, nil
}

func getEntityName(entity_type string, entity_id uuid.UUID, parentId *uuid.UUID) (string, error) {
	switch entity_type {
	case monitoring.NODE:
		entity, entityFetchErr := GetNode(entity_id)
		if entityFetchErr != nil {
			return "", fmt.Errorf("Unknown %v with id %v.Err %v", entity_type, entity_id, entityFetchErr)
		}
		return entity.Hostname, nil
	case monitoring.CLUSTER:
		entity, entityFetchErr := GetCluster(&entity_id)
		if entityFetchErr != nil {
			return "", fmt.Errorf("Unknown %v with id %v.Err %v", entity_type, entity_id, entityFetchErr)
		}
		return entity.Name, nil
	case monitoring.SLU:
		entity, entityFetchErr := GetSLU(parentId, entity_id)
		if entityFetchErr != nil {
			return "", fmt.Errorf("%v not a valid id of %v.Err %v", entity_id, entity_type, entityFetchErr)
		}
		return entity.Name, nil
	}
	return "", fmt.Errorf("Unsupported entity type %v", entity_type)
}

var entityParentMap = map[string]string{
	monitoring.SLU: monitoring.CLUSTER,
}

func getParentName(queriedEntityType string, parentId uuid.UUID) (string, error) {
	switch queriedEntityType {
	case monitoring.SLU:
		parent, parentFetchErr := GetCluster(&parentId)
		if parentFetchErr != nil {
			return "", fmt.Errorf("%v not a valid id of %v.Error %v", parentId, monitoring.CLUSTER, parentFetchErr)
		}
		return parent.Name, nil
	}
	return "", nil
}

func (a *App) GET_Utilization(w http.ResponseWriter, r *http.Request) {
	var start_time string
	var end_time string
	var interval string
	vars := mux.Vars(r)

	entity_id_str := vars["entity-id"]
	entity_type := vars["entity-type"]

	params := r.URL.Query()
	resource_name := params.Get("resource")
	duration := params.Get("duration")
	parent_id_str := params.Get("parent_id")

	entity_id, entityIdParseError := uuid.Parse(entity_id_str)
	if entityIdParseError != nil {
		HttpResponse(w, http.StatusBadRequest, entityIdParseError.Error())
		logger.Get().Error(entityIdParseError.Error())
		return
	}

	var parent_id *uuid.UUID
	var parentError error
	var parentName string
	if parent_id_str != "" {
		parent_id, parentError = uuid.Parse(parent_id_str)
		if parentError != nil {
			HttpResponse(w, http.StatusBadRequest, parentError.Error())
			logger.Get().Error(parentError.Error())
			return
		}

		parentName, parentError = getParentName(entity_type, *parent_id)
		if parentError != nil {
			HttpResponse(w, http.StatusBadRequest, parentError.Error())
			logger.Get().Error(parentError.Error())
			return
		}
	}

	entityName, entityNameError := getEntityName(entity_type, *entity_id, parent_id)
	if entityNameError != nil {
		HttpResponse(w, http.StatusBadRequest, entityNameError.Error())
		logger.Get().Error(entityNameError.Error())
		return
	}

	if duration != "" {
		if strings.Contains(duration, ",") {
			splt := strings.Split(duration, ",")
			start_time = splt[0]
			end_time = splt[1]
		} else {
			interval = duration
		}
	}

	paramsToQuery := map[string]interface{}{"nodename": entityName, "resource": resource_name, "start_time": start_time, "end_time": end_time, "interval": interval}
	if parentName != "" {
		paramsToQuery["parentName"] = parentName
	}

	res, err := GetMonitoringManager().QueryDB(paramsToQuery)
	if err == nil {
		json.NewEncoder(w).Encode(res)
	} else {
		HttpResponse(w, http.StatusInternalServerError, err.Error())
	}
}

//In memory ClusterId to ScheduleId map
var ClusterMonitoringSchedules map[uuid.UUID]uuid.UUID

func InitSchedules() {
	schedule.InitShechuleManager()
	if ClusterMonitoringSchedules == nil {
		ClusterMonitoringSchedules = make(map[uuid.UUID]uuid.UUID)
	}
	clusters, err := GetClusters()
	if err != nil {
		logger.Get().Error("Error getting the clusters list: %v", err)
		return
	}
	for _, cluster := range clusters {
		ScheduleCluster(cluster.ClusterId, cluster.MonitoringInterval)
	}
}

var mutex sync.Mutex

func SynchroniseScheduleMaintainers(clusterId uuid.UUID) (schedule.Scheduler, error) {
	mutex.Lock()
	defer mutex.Unlock()
	scheduler, err := schedule.NewScheduler()
	if err != nil {
		return scheduler, err
	}
	ClusterMonitoringSchedules[clusterId] = scheduler.Id
	return scheduler, nil
}

func ScheduleCluster(clusterId uuid.UUID, intervalInSecs int) {
	if intervalInSecs == 0 {
		intervalInSecs = monitoring.DefaultClusterMonitoringInterval
	}
	scheduler, err := SynchroniseScheduleMaintainers(clusterId)
	if err != nil {
		logger.Get().Error(err.Error())
	}
	f := GetApp().MonitorCluster
	go scheduler.Schedule(time.Duration(intervalInSecs)*time.Second, f, map[string]interface{}{"clusterId": clusterId})
}

func DeleteClusterSchedule(clusterId uuid.UUID) {
	mutex.Lock()
	defer mutex.Unlock()
	schedulerId, ok := ClusterMonitoringSchedules[clusterId]
	if !ok {
		logger.Get().Error("Cluster with id %v not scheduled", clusterId)
		return
	}
	if err := schedule.DeleteScheduler(schedulerId); err != nil {
		logger.Get().Error("Failed to delete schedule for cluster %v.Error %v", clusterId, err)
	}
	delete(ClusterMonitoringSchedules, clusterId)
}

func (a *App) MonitorCluster(params map[string]interface{}) {
	clusterId := params["clusterId"]
	id, ok := clusterId.(uuid.UUID)
	if !ok {
		logger.Get().Error("Failed to parse uuid")
		return
	}
	a.RouteProviderBasedMonitoring(id)
	return
}

func GetClusters() (models.Clusters, error) {
	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()

	collection := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_STORAGE_CLUSTERS)
	var clusters models.Clusters
	err := collection.Find(nil).All(&clusters)
	return clusters, err
}
