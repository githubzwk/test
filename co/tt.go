package s11580

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.ibm.com/ibm-privatecloud-databases/db2u-resources/pkg/builder/resources/account"
	"github.ibm.com/ibm-privatecloud-databases/db2u-resources/pkg/utils"

	"k8s.io/apimachinery/pkg/util/intstr"

	plumbingv1 "github.ibm.com/cdcp/apimachinery/pkg/env/plumbing/apis/databases.cloud.ibm.com/v1"
	"github.ibm.com/cdcp/plumbing/pkg/interfaces"
	"github.ibm.com/cdcp/plumbing/pkg/labels"
	plumbingresources "github.ibm.com/cdcp/plumbing/pkg/resources"
	app "github.ibm.com/ibm-privatecloud-databases/db2u-resources/pkg/builder/apps"
	"github.ibm.com/ibm-privatecloud-databases/db2u-resources/pkg/builder/core"
	resources2 "github.ibm.com/ibm-privatecloud-databases/db2u-resources/pkg/builder/resources"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

type Db2uResourceCollection struct {
	*ResourceCollection
	config           *plumbingv1.ResourceConfig
	environment      map[string]interface{}
	databases        []interface{}
	dbsStorageConfig map[string][]plumbingv1.StorageConfig
	podConfig        map[string]PodConfig
	advOpts          map[string]interface{}
	addOns           map[string]interface{}
	license          License
}

var podConfigNameReplace = map[string]map[string]string{
	"db2dv": {
		"hurricane": "hurricane-dv",
	},
}

func (e Db2uResourceCollection) Resources() []interfaces.Resource {
	resources := []interfaces.Resource{}
	if e.fmtn.Annotations == nil {
		e.fmtn.Annotations = map[string]string{}
	}
	var ok bool
	e.config, ok = e.fmtn.Spec.ResourceConfigs[DB2uRole]
	if !ok {
		klog.Error("Missing resource configs for " + DB2uRole)
		return nil
	}

	// --- set e.account rbac.PolicyRules  --- //
	e.account.AddRole(rbac.PolicyRule{
		Resources: []string{"pods/exec"},
		Verbs:     []string{"create"},
	})
	e.account.AddRole(rbac.PolicyRule{
		Resources: []string{"pods"},
		Verbs:     []string{"watch", "list", "get"},
	})
	if ConfigmapBackendDb2uAPI {
		e.account.AddRole(rbac.PolicyRule{
			Resources: []string{"configmaps"},
			Verbs:     []string{"watch", "list", "get", "update", "patch"},
		})
	}
	e.account.AddRole(rbac.PolicyRule{
		Resources: []string{"services"},
		Verbs:     []string{"watch", "list", "get"},
	})

	var err error

	// --- set e.environment --- //
	e.environment, err = utils.UnmarshalToMap(e.config.Env["config"])
	if err != nil {
		klog.Error("Unable to Unmarshal config", err.Error())
		return nil
	}

	e.environment["baseName"] = utils.BaseName(e.fmtn)

	if e.isDb2uClusterV2() {
		e.environment["db2ucluster"] = "v2"
		// --- set e.databases --- //
		jsonData, err := json.Marshal(e.environment["databases"])
		if err != nil {
			klog.Error("Unable to Marshal databases", err.Error())
			return nil
		}
		marshalledDbs := string(jsonData)
		e.databases, err = utils.UnmarshalToSlice(marshalledDbs)
		if err != nil {
			klog.Error("Unable to Unmarshal databases", err.Error())
			return nil
		}

		// --- set e.dbsStorageConfig --- //
		if err := json.Unmarshal([]byte(e.config.Env["dbStorageConfig"]), &e.dbsStorageConfig); err != nil {
			klog.Error("Unable to Unmarshal config", err.Error())
			return nil
		}
		// For single DB deployments (E.G. db2wh), set DB specific storage config from global storageConfig
		if len(e.dbsStorageConfig) == 0 {
			dbName := utils.GetValueWithDefaultValue(e.databases[0], "name", "bludb").(string)
			e.dbsStorageConfig[dbName] = e.config.Storage
		}
	}
	// --- set e.podConfig --- //
	e.podConfig = map[string]PodConfig{}
	if err := e.setPodConfig(); err != nil {
		return nil
	}

	// --- set e.advOpts --- //
	if ao, ok := e.config.Env["advOpts"]; ok && ao != "null" {
		if e.advOpts, err = utils.UnmarshalToMap(ao); err != nil {
			klog.Error("Unable to Unmarshal advOpts", err.Error())
			return nil
		}
	}

	// --- set e.addOns --- //
	e.addOns = map[string]interface{}{}
	if addOn, ok := e.config.Env["addOns"]; ok && addOn != "null" {
		if e.addOns, err = utils.UnmarshalToMap(addOn); err != nil {
			klog.Error("Unable to Unmarshal addOns", err.Error())
			return nil
		}
	}

	// --- set e.license --- //
	if l, ok := e.config.Env["license"]; ok && l != "null" {
		if err = json.Unmarshal([]byte(l), &e.license); err != nil {
			klog.Error("Unable to Unmarshal license", err.Error())
			return nil
		}
	}

	//--- PVCs ---//
	pvcList, scList := CreatePersistentVolumeClaimResources(e.fmtn, e.config.Storage)
	resourcesPVC := []interfaces.Resource{}
	for _, pvc := range pvcList {
		storageType := pvc.GetPersistentVolumeClaim().Annotations["storageConfigType"]
		if plumbingv1.StorageConfigType(storageType) == plumbingv1.StorageConfigTypeCreate {
			resourcesPVC = append(resourcesPVC, pvc)
		}
	}
	//ensure the order of PVC is 100% the same for every deployment
	sort.Slice(resourcesPVC, func(i, j int) bool {
		return resourcesPVC[i].Name() > resourcesPVC[j].Name()
	})
	resources = append(resources, resourcesPVC...)

	//--- Secrets ---//
	secretMap, secretVolume := e.createDb2uSecrets()
	resourcesSecret := []interfaces.Resource{}
	for _, value := range secretMap {
		resourcesSecret = append(resourcesSecret, value)
	}

	// ICD deployments, will not have any secrets in the resource config
	// and the db2u resource collection itself will create them
	resourcesSecret2, secretVolume2 := e.createDb2uSecretResources()
	resourcesSecret = append(resourcesSecret, resourcesSecret2...)
	secretVolume = append(secretVolume, secretVolume2...)

	//ensure the order of Secrets is 100% the same for every deployment
	sort.Slice(resourcesSecret, func(i, j int) bool {
		return resourcesSecret[i].Name() > resourcesSecret[j].Name()
	})
	resources = append(resources, resourcesSecret...)

	//--- ConfigMaps ---//
	cms, vols := e.createDb2uConfigmap()
	for _, value := range cms {
		resources = append(resources, value)
	}

	volumeList := e.createVolumeList(scList)
	//Add Db2u default Volume (Hostpath and emtpy dir)
	volumeList = append(volumeList, getDb2uDefaultVolume([]string{DB2Name})...)
	volumeList = append(volumeList, e.getDb2uProcVolume([]string{DB2NameInitKernel})...)
	if e.isSirius() {
		volumeList = append(volumeList, getDb2uDefaultVolume([]string{CatalogName})...)
		volumeList = append(volumeList, e.getDb2uProcVolume([]string{CatalogNameInitKernel})...)
	}
	if e.isDvBase() {
		volumeList = append(volumeList, getDvCachingDefaultVolume([]string{DVCachingName})...)
	}
	volumeList = append(volumeList, vols...)
	volumeList = append(volumeList, secretVolume...)
	// Add Volumes from external volume sources (ConfigsMaps or Secrets)
	extVols, err := e.getVolumesFromVolumeSource()
	if err != nil {
		klog.Error("unable to mount the external VolumeSource", err.Error())
	}
	volumeList = append(volumeList, extVols...)

	// Enable LDAP if specified (defaults to true to support stand-alone RHOS)
	ldapSvcName := ""
	ldapEnabledKey := "ldap.enabled"
	if e.isDb2uClusterV2() {
		ldapEnabledKey = "authentication.ldap.enabled"
	}
	if utils.GetValueWithDefaultValue(e.environment, ldapEnabledKey, true).(bool) {
		ldap := e.newLadp()
		ldapSvc := plumbingresources.NewClusterIPService(e.fmtn, LdapName, "",
			[]v1.ServicePort{{Name: LdapName, Protocol: v1.ProtocolTCP, Port: LdapPort}}, ldap.Labels(), ldap.Labels())

		resources = append(resources, ldapSvc, ldap)
		ldapSvcName = ldapSvc.Name()
	}

	//Create Jobs/Statefulset/Deployment

	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)

	// Deploy tools if MPP > 1 replica
	var tools *app.DeploymentBuilder
	// Logical and Physical MPP requires tools pod to be running for dns server and fcm support
	_, mlnTotal := e.getMlnTotal()
	if (dbType == "db2wh" || dbType == "sirius") && mlnTotal > 1 {
		toolsDeployment := e.newTools()
		tools = &toolsDeployment
	}
	toolsSvcName := ""
	if tools != nil {
		// Create tools, tools is also use for a local DNS server. This must be create before anything except account
		toolsSvc := plumbingresources.NewClusterIPService(e.fmtn, ToolsName, utils.BaseName(e.fmtn)+"-tools",
			[]v1.ServicePort{
				{Name: "dns-tcp", Protocol: v1.ProtocolTCP, Port: DnsPort, TargetPort: intstr.FromInt(DnsTgtPort)},
				{Name: "dns-udp", Protocol: v1.ProtocolUDP, Port: DnsPort, TargetPort: intstr.FromInt(DnsTgtPort)}},
			tools.Labels(), tools.Labels())
		toolsSvcName = toolsSvc.Name()
		if ldapSvcName != "" {
			tools.AddEnvToContainer(ToolsName,
				v1.EnvVar{Name: "LDAP_SERVICE_NAME", Value: ldapSvcName},
				v1.EnvVar{Name: "LDAP_SERVICE_PORT", Value: strconv.Itoa(int(LdapPort))})
		}
		resources = append(resources, toolsSvc, *tools)
	}

	_, isRestricted := e.checkAccount()

	// Deploy ETCD StatefulSet if not BigSQL/Dv
	var etcdendpoint string
	if e.isDb2uClusterV2() {
		etcdendpoint = utils.GetValueWithDefaultValue(e.environment, "etcd.endpoints", "").(string)
	}
	if etcdendpoint == "" {
		if dbType != "db2bigsql" && dbType != "db2dv" && (isRestricted == nil || *isRestricted == false) {
			// ETCD StatefulSet
			etcdSts := e.newETCDstatefulSet()
			etcdPorts := []v1.ServicePort{
				{Name: "etcd", Protocol: v1.ProtocolTCP, Port: EtcdClientPort},
				{Name: "peer", Protocol: v1.ProtocolTCP, Port: EtcdPeerPort},
			}
			etcdResource := []interfaces.Resource{}
			etcdSvc := plumbingresources.NewHeadlessService(e.fmtn, ETCDName, etcdPorts, etcdSts.Labels())
			// For Subdomain resolution we need to associate SVC name with the respective StateFulSet
			etcdSts.StatefulSet().Spec.ServiceName = etcdSvc.Name()
			e.fmtn.Spec.ResourceConfigs[DB2uRole].Env["etcdEndPoint"] = getETCDeP(&etcdSts, &etcdSvc, etcdPorts)
			etcdResource = []interfaces.Resource{etcdSvc, etcdSts}
			resources = append(resources, etcdResource...)
		}
	} else {
		e.fmtn.Spec.ResourceConfigs[DB2uRole].Env["etcdEndPoint"] = etcdendpoint
	}
	if e.isSirius() {
		siriusDts := e.newSiriusDts()
		dtsSvc := plumbingresources.NewClusterIPService(e.fmtn, DtsName, "",
			[]v1.ServicePort{
				{Name: "dts-server", Protocol: v1.ProtocolTCP, Port: DtsPort, TargetPort: intstr.FromInt(DtsTgtPort)},
			},
			siriusDts.Labels())
		resources = append(resources, dtsSvc, siriusDts)
	}

	// Db2U StatefulSet
	db2uNodePortSvc := plumbingresources.Service{}
	db2uHeadNodePortSvc := plumbingresources.Service{}
	db2uNodePortLabels := &labels.Labels{}
	db2uSts := app.StatefulSetBuilder{}
	db2uEngn := app.Db2uEngineBuilder{}
	apiEnvVar := []v1.EnvVar{}
	apiEnvVarCert := []v1.EnvVar{}
	db2uSvcLabels := &labels.Labels{}
	db2uStsLabel := labels.New()

	var disablenodeport string
	if e.isDb2uClusterV2() {
		disablenodeport = "disableNodePortService"
	} else {
		disablenodeport = "database.disableNodePortService"
	}

	if e.isSirius() {
		initialNodeNumber := 0
		resources = append(resources, e.newInstdb())
		memberSubsets := e.getMemberSubsets()
		for _, memberSubset := range memberSubsets {
			db2uIntSvc := e.newDb2uInternalService(memberSubset.Name+"-db2u-internal", db2uStsLabel)
			apiEnvVar = []v1.EnvVar{
				{Name: "DB2U_API_ENDPOINT", Value: db2uIntSvc.Name() + "." + e.fmtn.Namespace + ".svc" + ":50052"},
				{Name: "DB2U_API_KEY_FILE", Value: "/secrets/certs/db2u-api/tls.key"},
				{Name: "DB2U_API_CERT_FILE", Value: "/secrets/certs/db2u-api/tls.crt"}}

			db2uSts = e.newDb2uStatefulSet(db2uIntSvc.Name(), memberSubset.Name, memberSubset.Size, initialNodeNumber)
			db2uSts.AddEnvToAllContainer(apiEnvVar...)

			if tools != nil {
				tools.AddEnvToAllContainer(apiEnvVar...)
				nameServerVar := strings.ToUpper(strings.Replace(toolsSvcName, "-", "_", -1))
				db2uSts.AddEnvToContainer(DB2Name, v1.EnvVar{Name: "NAME_SERVER_VARIABLE", Value: nameServerVar + "_SERVICE_HOST"})
			}
			db2uStsLabel.AddMany(db2uSts.Labels().Map())
			db2uCISvc := plumbingresources.NewClusterIPService(e.fmtn, DB2Name, db2uSts.Name(),
				getDb2CIPorts(), db2uStsLabel.Clone())

			if !utils.GetValueWithDefaultValue(e.environment, disablenodeport, false).(bool) {
				db2uNodePortSvc = e.newDb2uNodePortService(memberSubset.Name+"-db2u-engn-svc", db2uSts.Labels().Clone())
				resources = append(resources, db2uIntSvc, db2uCISvc, db2uNodePortSvc, db2uSts)
			} else {
				db2uNodePortSvc = db2uCISvc
			}
			initialNodeNumber = initialNodeNumber + int(memberSubset.Size)
		}

		catalog, catalogSvc := e.newCatalog(apiEnvVar, apiEnvVarCert, toolsSvcName)
		resources = append(resources, catalogSvc, catalog)
	} else {
		db2uIntSvc := e.newDb2uInternalService("db2u-internal", db2uStsLabel)

		apiEnvVarCert = []v1.EnvVar{
			{Name: "DB2U_API_KEY_FILE", Value: "/secrets/certs/db2u-api/tls.key"},
			{Name: "DB2U_API_CERT_FILE", Value: "/secrets/certs/db2u-api/tls.crt"}}
		apiEnvVar = []v1.EnvVar{{Name: "DB2U_API_ENDPOINT", Value: db2uIntSvc.Name() + "." + e.fmtn.Namespace + ".svc" + ":50052"}}

		if ConfigmapBackendDb2uAPI {
			dbConfigmap := resources2.NewConfigMapWithValues(e.fmtn, utils.BaseName(e.fmtn)+"-db2u-api", nil)
			dbConfigmap.DisableUpdate()
			resources = append(resources, dbConfigmap)
			apiEnvVar = append(apiEnvVar, []v1.EnvVar{
				{Name: "DB2U_API_DATABASE_BACKEND", Value: "k8s"},
				{Name: "DB2U_API_CONFIGMAP_NAME", Value: dbConfigmap.Name()},
			}...)
		}
		db2uSts = e.newDb2uStatefulSet(db2uIntSvc.Name(), "", 0, 0)
		db2uSts.AddEnvToAllContainer(apiEnvVar...)
		db2uSts.AddEnvToAllContainer(apiEnvVarCert...)
		// Db2U Engn
		if e.isDb2uClusterV2() {
			db2uEngn = e.NewDb2uEngine(db2uIntSvc.Name())
			db2uEngn.AddEnvToAllContainer(apiEnvVar...)
			db2uEngn.AddEnvToAllContainer(apiEnvVarCert...)
		}
		if tools != nil {
			tools.AddEnvToAllContainer(apiEnvVar...)
			nameServerVar := strings.ToUpper(strings.Replace(toolsSvcName, "-", "_", -1))
			db2uSts.AddEnvToContainer(DB2Name, v1.EnvVar{Name: "NAME_SERVER_VARIABLE", Value: nameServerVar + "_SERVICE_HOST"})
			if e.isDb2uClusterV2() {
				db2uEngn.AddEnvToContainer(DB2Name, v1.EnvVar{Name: "NAME_SERVER_VARIABLE", Value: nameServerVar + "_SERVICE_HOST"})
			}
		}
		db2uStsLabel.AddMany(db2uSts.Labels().Map())
		db2uCISvc := plumbingresources.NewClusterIPService(e.fmtn, DB2Name, db2uSts.Name(),
			getDb2CIPorts(), db2uStsLabel.Clone())

		db2uSvcLabels = db2uSts.Labels().Clone()
		db2uNodePortLabels = db2uSvcLabels.Clone()
		if e.isDb2uClusterV2() {
			// skip adding ha_mode=primary to the selector and label of the internal services
			db2uEngn.Labels().Add("ha_mode", "primary")
			db2uNodePortLabels.Add("ha_mode", "primary")
		}

		if !utils.GetValueWithDefaultValue(e.environment, disablenodeport, false).(bool) {
			db2uNodePortSvc = e.newDb2uNodePortService("db2u-engn-svc", db2uNodePortLabels)
			db2uHeadNodePortLabels := db2uNodePortLabels.Clone()
			db2uHeadNodePortLabels.Add("name", "dashmpp-head-0")
			db2uHeadNodePortSvc = e.newDb2uNodePortService("db2u-head-engn-svc", db2uHeadNodePortLabels)
			resources = append(resources, db2uNodePortSvc)
			if !e.isBigSQLBase() { // db2uNodePortSvc selector already has label for head node
				resources = append(resources, db2uHeadNodePortSvc)
			}

		} else {
			db2uNodePortSvc = db2uCISvc
			db2uHeadNodePortSvc = db2uCISvc
		}

		// Add all the mandatory resources in the formation to resources slice
		if !e.isBigSQLBase() && !e.isDb2uClusterV2() {
			resources = append(resources, e.newInstdb())
		}

		// Add Db2 REST -- pass in the Db2 External SVC name
		if utils.GetValueWithDefaultValue(e.addOns, "rest.enabled", false).(bool) {
			rest := e.newRest(db2uNodePortSvc.Name())
			restNodePortSvc := plumbingresources.NewNodePortService(e.fmtn, "db2u-rest-svc",
				[]v1.ServicePort{
					{Name: "rest-server", Protocol: v1.ProtocolTCP, Port: RestServerPort},
				}, rest.Labels(), true)
			restDb2UExtNP := plumbingresources.NewExternalNetworkPolicyForRole(e.fmtn, []int32{RestServerPort}, rest.Labels(), "rest")
			resources = append(resources, restNodePortSvc, restDb2UExtNP, rest)
		}

		// Add Db2 Graph -- pass in the Db2 External SVC name
		if utils.GetValueWithDefaultValue(e.addOns, "graph.enabled", false).(bool) {
			graph := e.newGraph(db2uNodePortSvc.Name())
			graphNodePortSvc := plumbingresources.NewNodePortService(e.fmtn, "db2u-graph-svc",
				[]v1.ServicePort{
					{Name: "graph-server", Protocol: v1.ProtocolTCP, Port: GraphServerPort},
					{Name: "graph-ui", Protocol: v1.ProtocolTCP, Port: GraphUIPort},
				}, graph.Labels(), true)
			graphPorts := []int32{GraphServerPort, GraphUIPort}
			graphDb2UExtNP := plumbingresources.NewExternalNetworkPolicyForRole(e.fmtn, graphPorts, graph.Labels(), "graph")
			resources = append(resources, graphNodePortSvc, graphDb2UExtNP, graph)
		}

		resources = append(resources, db2uIntSvc, db2uCISvc)
		if e.isDb2uClusterV2() {
			resources = append(resources, db2uEngn)
		} else {
			resources = append(resources, db2uSts)
		}
	}

	// Add the metastoreUris as an env var if specified
	if e.isBigSQLBase() {
		if metastoreUris := utils.GetValueWithDefaultValue(e.advOpts, AdvanceOptionsMetastoreUrisFieldName, "").(string); metastoreUris != "" {
			db2uSts.AddEnvToContainer(DB2Name, v1.EnvVar{Name: ContainerMetastoreUrisEnvVarName, Value: metastoreUris})
		}
	}

	// Add Db2 QRep -- pass in the Db2 External SVC name for now
	if utils.GetValueWithDefaultValue(e.addOns, "qrep.enabled", false).(bool) && utils.GetValueWithDefaultValue(e.addOns, "qrep.license.accept", false).(bool) {
		qrep := e.newQrep(db2uNodePortSvc.Name())
		qrepRestNodePortSvc := plumbingresources.NewNodePortService(e.fmtn, "qrep-rest-svc",
			[]v1.ServicePort{
				{Name: "qrep-rest-api", Protocol: v1.ProtocolTCP, Port: QrepServerPort},
			}, qrep.Labels(), true)
		qrepMQNodePortSvc := plumbingresources.NewNodePortService(e.fmtn, "qrep-mq-svc",
			[]v1.ServicePort{
				{Name: "qrep-mq-sendq", Protocol: v1.ProtocolTCP, Port: QrepMqPortS},
				{Name: "qrep-mq-recvq", Protocol: v1.ProtocolTCP, Port: QrepMqPortR},
			}, qrep.Labels(), true)
		qrepPorts := []int32{QrepMqPortS, QrepMqPortR, QrepServerPort}
		qrepDb2UExtNP := plumbingresources.NewExternalNetworkPolicyForRole(e.fmtn, qrepPorts, qrep.Labels(), "qrep")
		resources = append(resources, qrepRestNodePortSvc, qrepMQNodePortSvc, qrepDb2UExtNP, qrep)
	}

	if dbType == "db2bigsql" {
		// allow connections between bigsql pods in an instance
		baseNP := plumbingresources.NewInternalIngressNetworkPolicy(db2uSts.Name()+"-np",
			e.fmtn, nil, db2uSts.Labels(), labels.New().Add("app", e.fmtn.Spec.ID), nil)

		// allow external connections to bigsql pods at 50000 (jdbc), 50001 (jdbc tls), 50052 (db2u api)
		ports := []int32{50000, 50001, 50052}
		db2uPortNP := plumbingresources.NewInternalIngressNetworkPolicy(db2uSts.Name()+"-port-np",
			e.fmtn, ports, db2uSts.Labels(), nil, nil)

		resources = append(resources, baseNP, db2uPortNP)
	}

	if dbType == "db2oltp" || dbType == "db2wh" {
		// Allow connections between Db2U pods only
		baseDb2UNP := plumbingresources.NewDefaultNetworkPolicy(e.fmtn, nil)

		//Allow external connection
		ports := []int32{50000, 50001, 50052}
		//Only use the following well-known labels in selector for Db2U NetworkPolicies
		db2uNpLabelFilter := []string{"app", "component", "formation_id", "role", "type"}
		db2uNpLabels := labels.New()
		db2uNpLabels = db2uNpLabels.AddMany(utils.FilterMapByKeys(db2uSts.Labels().Map(), db2uNpLabelFilter, false))
		BaseDb2UExtNetPol := plumbingresources.NewExternalDefaultNetworkPolicy(e.fmtn, ports, db2uNpLabels)

		resources = append(resources, baseDb2UNP, BaseDb2UExtNetPol)
	}

	if e.isBigSQLBase() || (e.isOpenDataFormatsEnabled() && !e.isDb2uClusterV2()) {
		db2uSvcLabels.Add("name", "dashmpp-head-0")
		db2uNodePortLabels.Add("name", "dashmpp-head-0")
		hurricane := e.newHurricane()
		hurricaneSvc := plumbingresources.NewHeadlessService(
			e.fmtn,
			HurricaneName,
			[]v1.ServicePort{
				{Name: "service", Protocol: v1.ProtocolTCP, Port: 7053},
				{Name: "admin", Protocol: v1.ProtocolTCP, Port: 7054},
				{Name: "thrift", Protocol: v1.ProtocolTCP, Port: 9083},
			}, hurricane.Labels())
		//Overwrite the private variable name which is only expose for ClusterIP.
		//This values much match the deployment hurricane name
		utils.UpdateVariable(&hurricaneSvc, "name", hurricane.Name())
		mysqlRootPassword, rootPasswordOk := e.config.Secrets["MYSQL_ROOT_PASSWORD"]
		mysqlPassword, mysqlPasswordOk := e.config.Secrets["MYSQL_PASSWORD"]
		if rootPasswordOk && mysqlPasswordOk {
			name := utils.BaseName(e.fmtn) + "-" + strings.ToLower(SqlName)
			mariadbSecret := resources2.NewSecretWithValues(e.fmtn, name, map[string]string{
				"MYSQL_ROOT_PASSWORD": mysqlRootPassword,
				"MYSQL_PASSWORD":      mysqlPassword,
			})
			hurricane.AddEnvToAllContainer(
				v1.EnvVar{Name: "MYSQL_USER", Value: "hive"},
				v1.EnvVar{Name: "MYSQL_DATABASE", Value: "metastore"},
				v1.EnvVar{Name: "MYSQL_ROOT_PASSWORD", ValueFrom: &v1.EnvVarSource{
					SecretKeyRef: &v1.SecretKeySelector{
						LocalObjectReference: v1.LocalObjectReference{Name: mariadbSecret.Name()},
						Key:                  "MYSQL_ROOT_PASSWORD",
					},
				}}, v1.EnvVar{Name: "MYSQL_PASSWORD", ValueFrom: &v1.EnvVarSource{
					SecretKeyRef: &v1.SecretKeySelector{
						LocalObjectReference: v1.LocalObjectReference{Name: mariadbSecret.Name()},
						Key:                  "MYSQL_PASSWORD",
					},
				}},
			)
			resources = append(resources, mariadbSecret)
		}
		hurricane.AddEnvToContainer(HurricaneName, apiEnvVar...)
		hurricane.AddEnvToContainer(HurricaneName, apiEnvVarCert...)

		// Add the metastoreUris as an env var if specified
		if e.isBigSQLBase() {
			if metastoreUris := utils.GetValueWithDefaultValue(e.advOpts, AdvanceOptionsMetastoreUrisFieldName, "").(string); metastoreUris != "" {
				hurricane.AddEnvToContainer(HurricaneName, v1.EnvVar{Name: ContainerMetastoreUrisEnvVarName, Value: metastoreUris})
			}
		}

		resources = append(resources, hurricane, hurricaneSvc)

		// add hurricane network policy
		if e.isBigSQLBase() {
			// allow connections from other pods in the same instance
			hurricaneNp := plumbingresources.NewInternalIngressNetworkPolicy(hurricane.Name()+"-np",
				e.fmtn, nil, hurricane.Labels(), labels.New().Add("app", e.fmtn.Spec.ID), nil)

			resources = append(resources, hurricaneNp)
		}
	}

	if isRestricted == nil || *isRestricted == false {
		resources = append(resources, e.newRestoreMorph(db2uSts.Name()))
	}

	if dbType == "db2dv" {
		dvutilsSts := e.newDVUtilsStatefulSet()
		dvutilsPorts := []v1.ServicePort{
			{Name: "namenode-rpc", Protocol: v1.ProtocolTCP, Port: 8020},
			{Name: "hive-metastore", Protocol: v1.ProtocolTCP, Port: 9083},
		}
		dvutilsSvc := plumbingresources.NewHeadlessService(e.fmtn, DVUtilsName, dvutilsPorts, dvutilsSts.Labels())
		dvutilsSts.StatefulSet().Spec.ServiceName = dvutilsSvc.Name()
		dvutilsSts.AddEnvToAllContainer(apiEnvVar...)

		//add dv-utils network policy
		dvutilsNp := plumbingresources.NewInternalIngressNetworkPolicy(dvutilsSts.Name()+"-np",
			e.fmtn, nil, dvutilsSts.Labels(), labels.New().Add("app", e.fmtn.Spec.ID), nil)

		dvcaching := e.newDVCaching()
		dvcaching.AddEnvToAllContainer(apiEnvVar...)
		dvcaching.AddEnvToAllContainer(v1.EnvVar{Name: "METADB_HOST", Value: db2uNodePortSvc.Name()})
		dvcachingPorts := []v1.ServicePort{
			{Name: "http", Protocol: v1.ProtocolTCP, Port: 8080},
			{Name: "https", Protocol: v1.ProtocolTCP, Port: 443, TargetPort: intstr.FromInt(int(8443))},
		}
		dvcachingSvc := plumbingresources.NewClusterIPService(e.fmtn, DVCachingName, "", dvcachingPorts, dvcaching.Labels())
		dvcaching.Deployment().Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}

		dvapi := e.newDVApi()
		dvapiPorts := []v1.ServicePort{
			{Name: "https", Protocol: v1.ProtocolTCP, Port: 3300},
		}
		dvapiSvc := plumbingresources.NewClusterIPService(e.fmtn, DVApiName, "", dvapiPorts, dvapi.Labels())

		// db2uSvcLabels will select only the head pods, gets "dashmpp-head-0" added in the hurricane section above
		dvAPIEngineSvc := plumbingresources.NewClusterIPService(e.fmtn, "dv-api-engine", "",
			[]v1.ServicePort{{Name: "dv-api-engine", Protocol: v1.ProtocolTCP, Port: 3303}}, db2uSvcLabels.Clone())

		zenServiceInstanceId := utils.GetValueWithDefaultValue(e.advOpts, "zenServiceInstanceId", "1619475870630718").(string)
		db2uSts.AddEnvToAllContainer(v1.EnvVar{Name: "ZEN_SERVICE_INSTANCE_ID", Value: zenServiceInstanceId})
		db2uEngn.AddEnvToAllContainer(v1.EnvVar{Name: "ZEN_SERVICE_INSTANCE_ID", Value: zenServiceInstanceId})
		dvapi.AddEnvToAllContainer(v1.EnvVar{Name: "ZEN_SERVICE_INSTANCE_ID", Value: zenServiceInstanceId})

		resources = append(resources, dvutilsSts, dvutilsSvc, dvutilsNp, dvcaching, dvcachingSvc, dvAPIEngineSvc, dvapi, dvapiSvc)
	}

	//Check if we should inject DNS.

	//This object could be null as it need some more info from Db2uCluster.
	//If the info is not pass down, assume no upgrade jobs need to be deploy
	upgradeDb2EngineJob := e.NewDb2uUpdateJobs(db2uSts.Name())
	if upgradeDb2EngineJob.PodBuilder != nil {
		resources = append(resources, upgradeDb2EngineJob)
	}

	var etcdAffinity v1.Affinity
	var etcdTolerations []v1.Toleration
	if e.isDb2uClusterV2() {
		etcdAffinity = e.getEtcdAffinity()
		etcdTolerations = e.getEtcdTolerations()
	}

	builders := FindAllBuilder(resources)
	tempBuilder := FindAllTemplateBuilder(resources)

	//Apply Affinity to all.
	for _, bu := range builders {
		if !e.skipAddAfinity(bu) {
			if bu.Spec.Affinity != nil {
				bu.Spec.Affinity = utils.MergeAffinity(*bu.Spec.Affinity, *utils.WithDefaultAffinity(e.fmtn, DB2uRole))
			} else {
				bu.Spec.Affinity = utils.WithDefaultAffinity(e.fmtn, DB2uRole)
			}
			bu.Spec.Tolerations = e.config.Tolerations
		} else if (etcdAffinity != v1.Affinity{}) {
			if bu.Spec.Affinity != nil {
				bu.Spec.Affinity = utils.MergeAffinity(*bu.Spec.Affinity, etcdAffinity)
			} else {
				bu.Spec.Affinity = &etcdAffinity
			}
			if etcdTolerations != nil {
				bu.Spec.Tolerations = etcdTolerations
			}
		}
	}

	for _, volume := range volumeList {

		for _, match := range volume.Visibility {
			//If storage is not VolumeClaimTemplates
			if volume.Type != plumbingv1.StorageConfigTypeTemplate {
				for _, podbuilder := range FindAllPodBuilderWithContainerName(builders, match) {
					podbuilder.AddVolumeToContainer(match, volume.VolumeMount, volume.VolumeSource)
				}
			} else {
				//VolumeClaimTemplates can only be handle by ss
				//Find the PVC which need to be added to the statefulset
				//Find the statefulset which the volume request to be added to.
				var (
					r           *core.PersistentVolumeClaim
					ok          bool
					storageType string
				)
				if r, ok = pvcList[volume.VolumeMount.Name]; !ok {
					continue
				}
				if storageType, ok = r.GetPersistentVolumeClaim().Annotations["storageConfigType"]; !ok {
					continue
				}
				if plumbingv1.StorageConfigType(storageType) == plumbingv1.StorageConfigTypeTemplate {
					for _, ss := range tempBuilder {
						ss.HandleTemplate(match, volume.VolumeMount, *r.GetPersistentVolumeClaim())
					}
				}
			}
		}
	}

	for idx := range e.account.SecurityContextResources {
		if !e.account.SecurityContextResources[idx].IsThisSecurityPolicyTypeValid(account.SecurityContextConstraintType) {
			continue
		}
		if isRestricted != nil && *isRestricted {
			e.account.SecurityContextResources[idx].SetSecurityPolicy(RestrictedDb2uSCC)
		}
	}

	//SecurityContext, sc check for Volume, all Volume should be place before we process any sc
	for _, builder := range builders {
		for idx := range e.account.SecurityContextResources {
			e.account.SecurityContextResources[idx].ProcessSecurityContextFromPods(builder.Spec)
		}
		if dbType == "db2oltp" {
			if strings.HasSuffix(builder.Name(), Name) {
				builder.Annotations().AddMany(Db2LicenseAnnotations)
			}
		} else if dbType == "db2wh" {
			if strings.HasSuffix(builder.Name(), Name) {
				builder.Annotations().AddMany(Db2WhLicenseAnnotations)
			}
		}

		// Dynamically add Labels, Annotations or Resource requests/limits

		for podName, podCfg := range e.podConfig {
			//PodName for hurricane is not 100% the name.
			if newNameMap, ok := podConfigNameReplace[dbType]; ok {
				if newName, ok := newNameMap[podName]; ok {
					podName = newName
				}
			}
			if strings.HasSuffix(builder.Name(), podName) {
				// Inject the custom labels in podConfig
				if podCfg.Labels != nil {
					builder.Labels().AddMany(podCfg.Labels)
				}
				// Inject the custom Annotation in podConfig
				if podCfg.Annotations != nil {
					builder.Annotations().AddMany(podCfg.Annotations)
				}
				if podCfg.HostAliases != nil {
					builder.Spec.HostAliases = podCfg.HostAliases
				}
				// Inject the custom resource requirements (done at container level)
				// Overwrite for resource requirements
				if podCfg.ResourceRequirements != nil {
					for cn, res := range podCfg.ResourceRequirements {
						if container := builder.GetContainer(cn); container != nil {
							container.Resources = res
						}
					}
				}
			}
		}
		builder.Sync()
	}

	for _, builder := range FindAllSyncBuilder(resources) {
		builder.Sync()
	}
	return resources
}

func (e Db2uResourceCollection) newSiriusDts() app.DeploymentBuilder {
	deployment := app.NewDeployment(e.fmtn, DtsName)
	deployment.SetRole(DtsName)
	deployment.WaitForReady(false)
	deployment.SetReplicas(1)
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = NonrootPodSecurityContext.DeepCopy()

	c := app.NewContainer(DtsName).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "FDB_SERVICE", Value: utils.BaseName(e.fmtn)},
			{Name: "FDB_TLS_CERTIFICATE_FILE", Value: "/var/fdb-certs/tls.crt"},
			{Name: "FDB_TLS_KEY_FILE", Value: "/var/fdb-certs/tls.key"},
			{Name: "FDB_TLS_CA_FILE", Value: "/var/fdb-certs/ca.crt"},
		}...).
		SetImage(e.config.Images[DtsName]).
		SetResourceRequirements(DtsResource).
		SetSecurityContext(NonRootSecurityContext).
		ToContainer()

	c.ReadinessProbe = &ReadinessProbeDts
	c.LivenessProbe = &LivenessProbeDts

	deployment.AddContainer(c)
	deployment.WaitForReady(false)

	return deployment
}

func (e Db2uResourceCollection) newDb2uNodePortService(name string, labels *labels.Labels) plumbingresources.Service {
	db2uSvcPorts := []v1.ServicePort{
		{Name: "legacy-server", Protocol: v1.ProtocolTCP, Port: Db2Port},
		{Name: "ssl-server", Protocol: v1.ProtocolTCP, Port: Db2SslPort},
	}
	if e.isDvBase() {
		db2uSvcPorts = append(db2uSvcPorts, v1.ServicePort{Name: "qpdiscovery", Protocol: v1.ProtocolTCP, Port: DVDiscoveryPort})
	}

	db2uNodePortSvc := plumbingresources.NewNodePortService(e.fmtn, name,
		db2uSvcPorts, labels, true)

	return db2uNodePortSvc
}

func (e Db2uResourceCollection) newDb2uInternalService(name string, labels *labels.Labels) plumbingresources.Service {
	db2uIntSvc := plumbingresources.NewHeadlessService(
		e.fmtn,
		name,
		[]v1.ServicePort{
			{Name: "main", Protocol: v1.ProtocolTCP, Port: Db2Port},
			{Name: "wvha-rest", Protocol: v1.ProtocolTCP, Port: WvHaRestPort},
			{Name: "db2uapi", Protocol: v1.ProtocolTCP, Port: Db2uapi},
		}, labels)

	return db2uIntSvc
}

//-------- Db2u engine StatefulSet and helper func ---------

func (e Db2uResourceCollection) newDb2uStatefulSet(serviceName string, subsetName string, subsetSize int32, initialNodeNumber int) app.StatefulSetBuilder {
	statefulsetName := DB2Name
	if subsetSize > 0 {
		statefulsetName = subsetName + "-" + statefulsetName
	}
	ss := app.NewStatefulSet(e.fmtn, statefulsetName, DB2uRole)
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)
	hostIPC := utils.GetValueWithDefaultValue(e.advOpts, "hostIPC", false).(bool)
	mlnTotalString, _ := e.getMlnTotal()
	_, mlnTotal := e.getMlnTotal()
	db2Replicas := int(e.config.Replicas)
	if e.isShutdown() {
		db2Replicas = 0
	}

	setPodSysctls, isRestricted := e.checkAccount()

	ss.SetReplicas(db2Replicas)
	if subsetSize > 0 {
		ss.SetReplicas(int(subsetSize))
	}

	ss.AddLabels("type", "engine").
		AddLabels("component", dbType).
		SetRole("db")
	ss.SetServiceAccount(e.account.Name())

	initLabelSC := InitLabelSecurityContext
	if isRestricted != nil && *isRestricted {
		ss.Spec.SecurityContext = RestrictedPodSecurityContext.DeepCopy()
		initLabelSC = NonRootRestrictedSecurityContext
	} else {
		ss.Spec.SecurityContext = NonrootPodSecurityContext.DeepCopy()
	}

	// If privileged, then we can set IPC Kernel params via the init Container
	// If spec.account or spec.account.privileged is not specified assume privileged
	initCmdDb2Kernel := initCommandDB2Kernel
	initKernelSC := InitKernelSecurityContext

	Db2uResource := Db2uWhSmpResource
	if mlnTotal > 1 || db2Replicas > 1 {
		Db2uResource = Db2uMppResource
	} else if dbType == "db2oltp" {
		Db2uResource = Db2uOltpResource
	}

	// Inject Sysctls into POD spec if specified
	if setPodSysctls != nil && *setPodSysctls {
		// Run set_kernel_prams.sh in validate mode and hence, use Non-root SC
		initCmdDb2Kernel = initCommandDB2Kernel + " --validate"
		initKernelSC = NonRootRestrictedSecurityContext

		// Inject sysctls into POD SC only for physical smp
		if mlnTotal == 1 {
			var limits v1.ResourceList
			if podcfgr, ok := e.podConfig[DB2Name]; ok {
				if rr, ok := podcfgr.ResourceRequirements[DB2Name]; ok {
					limits = rr.Limits
					if limits == nil {
						limits = rr.Requests
					}
				}
			}
			if limits == nil {
				limits = Db2uResource.Limits
			}
			ss.Spec.SecurityContext.Sysctls = GetDb2PodSccSysctls(int(limits.Memory().Value()))
		}
	}

	c := e.newDb2InitKernelContainer(DB2NameInitKernel, DB2Name, ss.Name(), initCmdDb2Kernel, initKernelSC)

	numReplicas := e.config.Replicas
	if e.isSirius() {
		numReplicas = e.getTotalMemberSubsetNodeCount()
	}

	c2 := app.NewContainer(DB2NameInitLabel).
		SetCommand([]string{"bash", "-ec", "/tools/pre-install/db2u_init.sh ${MLN_TOTAL} ${REPLICAS}"}).
		SetResourceRequirements(Db2uResourceInit).
		AddDefaultEnvironment(e.fmtn, ss.Name()).
		SetSecurityContext(initLabelSC).
		SetImage(e.config.Images[ToolsName]).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "DB2TYPE", Value: strings.ToLower(dbType)},
			{Name: "POD_NAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "MLN_TOTAL", Value: mlnTotalString},
			{Name: "REPLICAS", Value: fmt.Sprint(numReplicas)},
		}...)

	imageName := DB2Name
	db2SecurityContext := Db2SecurityContext
	if isRestricted != nil && *isRestricted {
		imageName = DB2RestrictedName
		db2SecurityContext = Db2RestrictedSecurityContext
	} else if e.isBigSQLBase() || e.isOpenDataFormatsEnabled() {
		imageName = DB2WatsonQueryName
	}
	if e.isSirius() {
		imageName = DB2SiriusName
		Db2uResource = Db2uSiriusResource
	}

	c3 := app.NewContainer(DB2Name).
		SetResourceRequirements(Db2uResource).
		SetSecurityContext(db2SecurityContext).
		SetImage(e.config.Images[imageName]).
		AddPorts(true, []v1.ContainerPort{
			{Name: "db2-server", ContainerPort: Db2Port, Protocol: v1.ProtocolTCP},
			{Name: "db2-ssl-server", ContainerPort: Db2SslPort, Protocol: v1.ProtocolTCP},
			{Name: "db2uapi", ContainerPort: Db2uapi, Protocol: v1.ProtocolTCP},
		}...).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "POD", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.labels['name']"}}},
			{Name: "POD_NAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "MEMORY_REQUEST", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{Resource: "requests.memory"}}},
			{Name: "MEMORY_LIMIT", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{Resource: "limits.memory"}}},
			{Name: "CPU_LIMIT", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{Resource: "limits.cpu"}}},
		}...)
	if etcdEndpoint, ok := e.fmtn.Spec.ResourceConfigs[DB2uRole].Env["etcdEndPoint"]; ok {
		c3.AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "etcdoperator", Value: "true"},
			//etcdctl env vars
			{Name: "WV_RECOVERY", Value: "partial"},
			{Name: "WV_HACLASS", Value: "UDB"},
			{Name: "ETCD_ENDPOINTS", Value: etcdEndpoint},
			//etcdctl env vars
			{Name: "ETCDCTL_API", Value: "3"},
			{Name: "ETCDCTL_ENDPOINTS", Value: etcdEndpoint},
		}...)
	}
	//Add in the readiness probe and lifecycle
	if isRestricted != nil && *isRestricted {
		c3.Lifecycle = DB2uRestrictedLifecycle
		c3.ReadinessProbe = &ReadinessProbeDB2uRestricted
		c3.LivenessProbe = &LivenessProbeDB2uRestricted
		c3.StartupProbe = &StartupProbeDB2uRestricted
	} else {
		c3.ReadinessProbe = &ReadinessProbeDB2u
	}

	if e.isDvBase() {
		c3.LivenessProbe = &LivenessProbeDB2dv
	} else if e.isBigSQLBase() {
		c3.LivenessProbe = &LivenessProbeDB2Bigsql
	}

	if mlnTotal > 1 {
		// Physical MPP only
		if db2Replicas > 1 {
			sshPort := v1.ContainerPort{Name: "ssh-port", ContainerPort: Db2uSshPort, Protocol: v1.ProtocolTCP}
			if !e.isBigSQLBase() || hostIPC {
				sshPort.HostPort = Db2uSshPort
			}
			c3.AddPorts(true, []v1.ContainerPort{
				sshPort,
				{Name: "wvha-mgmt-port", ContainerPort: WvHaMgmtPort, Protocol: v1.ProtocolTCP},
				{Name: "wvha-rest-port", ContainerPort: WvHaRestPort, Protocol: v1.ProtocolTCP},
			}...)

		}

		// Logical and Physical MPP. We need to enable hostIPC and add FCM ports. Since hostIPC is enabled, logical MPP requires AntiAffinity
		c3.AddPortsRange(v1.ContainerPort{Name: "db2-fcm-p", Protocol: v1.ProtocolTCP, ContainerPort: Db2FcmStartPort}, mlnTotal, 1)
		if !e.isBigSQLBase() || hostIPC {
			ss.Spec.HostIPC = true
			ss.Spec.Affinity = &v1.Affinity{
				PodAntiAffinity: &v1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
						{
							TopologyKey: "kubernetes.io/hostname",
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"type": "engine"},
							},
						},
					},
				},
			}
		}

		// Add FCM requirements if present in the CRD
		if fcmStruct, ok := e.environment["fcm"]; ok && fcmStruct != "null" {
			hostNetwork := utils.GetValueWithDefaultValue(e.environment, "fcm.hostNetwork", false).(bool)
			dnsPolicy := utils.GetValueWithDefaultValue(e.environment, "fcm.dnsPolicy", v1.DNSNone).(string)
			ss.Spec.HostNetwork = hostNetwork
			ss.Spec.DNSPolicy = v1.DNSPolicy(dnsPolicy)
		}

	}

	if serviceName == "" {
		serviceName = ss.Name()
	}
	ss.Spec.DNSConfig = &v1.PodDNSConfig{Options: []v1.PodDNSConfigOption{{Name: "ndots", Value: utils.PointerString("2")}}}
	if e.isBigSQLBase() {
		ss.AddInitContainer(e.newInstdb().GetContainer(SqllibSharedName))
	}
	ss.AddInitContainer(c2.ToContainer())
	ss.AddInitContainer(c.ToContainer())
	ss.AddContainer(c3.ToContainer())
	ss.StatefulSet().Spec.ServiceName = serviceName
	ss.AddEnvToAllContainer(v1.EnvVar{Name: "SERVICE_NAME", Value: serviceName})
	if subsetSize > 0 {
		ss.AddEnvToAllContainer(v1.EnvVar{Name: "INITIAL_NODE_NUMBER", Value: strconv.Itoa(initialNodeNumber)})
		ss.AddEnvToAllContainer(v1.EnvVar{Name: "SUBSET_NAME", Value: subsetName})
	}

	// Don't wait until ready if using dedicated catalog node.
	// The engine node won't be ready until the catalog node starts.
	if e.isSirius() {
		ss.WaitForReady(false)
	} else {
		ss.WaitForReady(true)
	}

	if e.isBigSQLBase() {
		ss.Spec.HostIPC = hostIPC
	}
	return ss
}

func (e Db2uResourceCollection) newDb2InitKernelContainer(initContainerName string, containerName string, serviceName string, command string, securityContext v1.SecurityContext) *app.ContainerBuilder {
	c := app.NewContainer(initContainerName).
		SetCommand([]string{"bash", "-ec", command}).
		SetResourceRequirements(Db2uResourceInit).
		AddDefaultEnvironment(e.fmtn, serviceName).
		SetSecurityContext(securityContext).
		SetImage(e.config.Images[ToolsName]).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "POD_NAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "MEMORY_LIMIT", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{ContainerName: containerName, Resource: v1.ResourceLimitsMemory.String()}}},
		}...)

	return c
}

func (e Db2uResourceCollection) newDb2EngineContainer(containerName string, resourceRequirements v1.ResourceRequirements, securityContext v1.SecurityContext, imageName string, isRestricted *bool) *app.ContainerBuilder {
	c := app.NewContainer(containerName).
		SetResourceRequirements(resourceRequirements).
		SetSecurityContext(securityContext).
		SetImage(e.config.Images[imageName]).
		AddPorts(true, []v1.ContainerPort{
			{Name: "db2-server", ContainerPort: Db2Port, Protocol: v1.ProtocolTCP},
			{Name: "db2-ssl-server", ContainerPort: Db2SslPort, Protocol: v1.ProtocolTCP},
			{Name: "db2uapi", ContainerPort: Db2uapi, Protocol: v1.ProtocolTCP},
		}...).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "POD", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.labels['name']"}}},
			{Name: "POD_NAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "MEMORY_REQUEST", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{Resource: "requests.memory"}}},
			{Name: "MEMORY_LIMIT", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{Resource: "limits.memory"}}},
			{Name: "CPU_LIMIT", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{Resource: "limits.cpu"}}},
		}...)
	if etcdEndpoint, ok := e.fmtn.Spec.ResourceConfigs[DB2uRole].Env["etcdEndPoint"]; ok {
		c.AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "etcdoperator", Value: "true"},
			//etcdctl env vars
			{Name: "WV_RECOVERY", Value: "partial"},
			{Name: "WV_HACLASS", Value: "UDB"},
			{Name: "ETCD_ENDPOINTS", Value: etcdEndpoint},
			//etcdctl env vars
			{Name: "ETCDCTL_API", Value: "3"},
			{Name: "ETCDCTL_ENDPOINTS", Value: etcdEndpoint},
		}...)
	}

	if e.isSirius() {
		c.AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "FDB_TLS_CERTIFICATE_FILE", Value: "/secrets/db2ssl/tls.crt"},
			{Name: "FDB_TLS_KEY_FILE", Value: "/secrets/db2ssl/tls.key"},
			{Name: "FDB_TLS_CA_FILE", Value: "/secrets/db2ssl/ca.crt"},
			{Name: "FDB_CLUSTER_FILE", Value: "/mnt/blumeta0/home/db2inst1/cluster-file"},
			{Name: "IS_USING_DEDICATED_CATALOG_NODE", Value: "true"},
		}...)
	} else {
		c.AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "IS_USING_DEDICATED_CATALOG_NODE", Value: "false"},
		}...)
	}

	//Add in the readiness probe and lifecycle
	if isRestricted != nil && *isRestricted {
		c.Lifecycle = DB2uRestrictedLifecycle
		c.ReadinessProbe = &ReadinessProbeDB2uRestricted
		c.LivenessProbe = &LivenessProbeDB2uRestricted
		c.StartupProbe = &StartupProbeDB2uRestricted
	} else {
		c.ReadinessProbe = &ReadinessProbeDB2u
	}

	if e.isDvBase() {
		c.LivenessProbe = &LivenessProbeDB2dv
	}

	return c
}

func (e Db2uResourceCollection) isSirius() bool {
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)
	return strings.ToLower(dbType) == "sirius"
}

func (e Db2uResourceCollection) getTotalMemberSubsetNodeCount() int32 {
	result := int32(0)
	for _, memberSubset := range e.getMemberSubsets() {
		result += memberSubset.Size
	}
	return result
}

func (e Db2uResourceCollection) isDb2uClusterV2() bool {
	if v, ok := e.config.Env["db2ucluster/v2"]; ok {
		if v == "true" {
			return true
		} else {
			return false
		}
	} else {
		return false
	}
}

func (e Db2uResourceCollection) isShutdown() bool {
	if v, ok := e.config.Env["shutdown"]; ok {
		if v == "true" {
			return true
		} else {
			return false
		}
	} else {
		return false
	}
}

func (e Db2uResourceCollection) isBigSQLBase() bool {
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)
	return strings.ToLower(dbType) == "db2bigsql" || strings.ToLower(dbType) == "db2dv"
}

// isOpenDataFormatsEnabled checks if Open Data Formats are supported (aka hurricane, aka bigsql)
func (e Db2uResourceCollection) isOpenDataFormatsEnabled() bool {
	if h, ok := e.config.Env["hurricane"]; ok && h == "true" {
		return true
	}
	return utils.GetValueWithDefaultValue(e.addOns, "opendataformats.enabled", false).(bool)
}

func (e Db2uResourceCollection) getDb2uProcVolume(visibility []string) []Volume {
	v := []Volume{}
	procVol := Volume{
		Visibility:   visibility,
		VolumeMount:  v1.VolumeMount{Name: "proc", MountPath: "/host/proc", ReadOnly: false},
		VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/proc"}},
	}

	if setPodSysctls, _ := e.checkAccount(); setPodSysctls == nil || *setPodSysctls == false {
		v = append(v, procVol)
	}

	return v
}

// Return MLN total as a string and also in integer format
func (e Db2uResourceCollection) getMlnTotal() (string, int) {
	var mlnTotalString string
	if e.isDb2uClusterV2() {
		mlnTotalString = fmt.Sprint(utils.GetValueWithDefaultValue(e.environment, "partitionConfig.total", 1))
	} else {
		mlnTotalString = fmt.Sprint(utils.GetValueWithDefaultValue(e.environment, "mln.total", 1))
	}

	mlnTotal, _ := strconv.Atoi(mlnTotalString)

	if e.isSirius() {
		mlnTotal = int(e.getTotalMemberSubsetNodeCount())
		mlnTotalString = fmt.Sprint(mlnTotal)
	}

	return mlnTotalString, mlnTotal
}

// generate Db2 ClusterIP Ports
func getDb2CIPorts() []v1.ServicePort {
	db2CIPorts := []v1.ServicePort{
		{Name: "db2-server", Protocol: v1.ProtocolTCP, Port: Db2Port},
		{Name: "db2-ssl-server", Protocol: v1.ProtocolTCP, Port: Db2SslPort},
	}
	sparkPort := SparkStartPort
	for i := 0; i <= 5; i++ {
		db2CIPorts = append(db2CIPorts, v1.ServicePort{Name: "spark-p" + strconv.Itoa(sparkPort), Protocol: v1.ProtocolTCP, Port: int32(sparkPort)})
		sparkPort++
	}
	return db2CIPorts
}

// Check account in spec to get: a) if privileged -- required to throttle POD level sysctl injection
// b) if restricted -- required to set imaged to Non-root one, and return 2 *booleans.
func (e Db2uResourceCollection) checkAccount() (*bool, *bool) {
	setPodSysctls, db2Restricted := false, false

	if acc, err := utils.UnmarshalToMap(e.fmtn.Spec.ResourceConfigs[DB2uRole].Env["account"]); err == nil {
		if e.isDb2uClusterV2() {
			jsonData, err := json.Marshal(acc["securityConfig"])
			if err != nil {
				klog.Error("Unable to Marshal instance", err.Error())
				return &setPodSysctls, &db2Restricted
			}
			stringData := string(jsonData)
			if secCfg, err := utils.UnmarshalToMap(stringData); err == nil {
				if values, ok := secCfg["privilegedSysctlInit"]; ok && !values.(bool) {
					setPodSysctls = true
				}
				if values, ok := secCfg["nonRootInstall"]; ok && values.(bool) {
					db2Restricted = true
				}
			}
		} else {
			if values, ok := acc["privileged"]; ok && !values.(bool) {
				setPodSysctls = true
			}
			if values, ok := acc["restricted"]; ok && values.(bool) {
				db2Restricted = true
			}
		}
	}

	return &setPodSysctls, &db2Restricted
}

// Return the instdb image to use based on: (a) non-root or root install (b) FUTURE: dbtype
func (e Db2uResourceCollection) getInstdbImgName() string {
	instdbImageName := instdbImageMap["common"]

	if _, isRestricted := e.checkAccount(); isRestricted != nil && *isRestricted {
		instdbImageName = instdbImageMap["restricted"]
	}

	if e.isSirius() {
		instdbImageName = instdbImageMap["sirius"]
	}

	/* TODO - when/if we have instb-per-dbtype
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "unknown").(string)
	instdbImageName =  instdbImageMap[dbType]

	*/

	return instdbImageName
}

// Get resource requirements for each POD via podConfig in spec and merge with default Resource
// Requirements in order to override only a subset of resources (CPU/Memory/Ephemeral Storage).
func (e Db2uResourceCollection) setPodConfig() error {
	// --- set e.podConfig --- //
	if e.podConfig == nil {
		e.podConfig = map[string]PodConfig{}
	}
	if pc, ok := e.config.Env["podConfig"]; ok && pc != "null" {
		if err := json.Unmarshal([]byte(pc), &e.podConfig); err != nil {
			klog.Error("Unable to Unmarshal podConfig", err.Error())
			return nil
		}
	}
	// If no PodConfig defined in CRD nothing to set here
	if len(e.podConfig) == 0 {
		return nil
	}

	// Update podConfig for Db2u POD, which is handled outside the fixed Resource Requirements
	// defined in defaultRRmap, since we have different defaults for OLTP vs WH
	_, mlnTotal := e.getMlnTotal()
	db2Replicas := int(e.config.Replicas)
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)
	Db2uResource := Db2uWhSmpResource
	if mlnTotal > 1 || db2Replicas > 1 {
		Db2uResource = Db2uMppResource
	} else if dbType == "db2oltp" {
		Db2uResource = Db2uOltpResource
	}
	if pc, ok := e.podConfig[DB2Name]; ok && pc.ResourceRequirements != nil {
		res := pc.ResourceRequirements[DB2Name]
		pc.ResourceRequirements[DB2Name] = utils.MergeResourceRequirements(&Db2uResource, &res, utils.IsResourceLimitsSameAsRequests(&Db2uResource))
	}

	// Merge with default Resource Requirements for all PODs, except Db2u POD
	for pn, pc := range e.podConfig {
		if rrDef, ok := defaultRRmap[pn]; ok && pc.ResourceRequirements != nil {
			res := pc.ResourceRequirements[pn]
			pc.ResourceRequirements[pn] = utils.MergeResourceRequirements(&rrDef, &res, utils.IsResourceLimitsSameAsRequests(&rrDef))
		}
	}

	return nil
}

// ------------------ ETCD --------------------
func (e Db2uResourceCollection) newETCDstatefulSet() app.StatefulSetBuilder {
	// Create the base ETCD StateFulSet
	ss := app.NewStatefulSet(e.fmtn, ETCDName, ETCDRole)

	etcdReplicas := 1
	if e.isDb2uClusterV2() {
		etcdReplicas = int(e.fmtn.Spec.ResourceConfigs["etcd-m"].Replicas)
	} else {
		if e.config.Replicas >= 3 {
			etcdReplicas = 3
		}
	}
	if e.isShutdown() {
		etcdReplicas = 0
	}

	runtimeEnv := utils.GetValueWithDefaultValue(e.advOpts, "runtimeEnv", "LOCAL").(string)

	ss.AddLabels("component", ETCDName)
	ss.WaitForReady(false)
	ss.SetReplicas(etcdReplicas)
	ss.SetServiceAccount(e.account.Name())
	ss.Spec.SecurityContext = &EtcdPodSecurityContext

	c := app.NewContainer(ETCDInit).
		SetCommand([]string{"bash", "-c", "/scripts/init.sh"}).
		SetResourceRequirements(ETCDResourceInit).
		AddDefaultEnvironment(e.fmtn, ss.Name()).
		SetImage(e.config.Images[ETCDName]).
		SetSecurityContext(EtcdNonRootSecurityContext).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "INITIAL_CLUSTER_SIZE", Value: strconv.Itoa(etcdReplicas)},
		}...)

	c2 := app.NewContainer(ETCDName).
		SetCommand([]string{"/scripts/start.sh"}).
		SetResourceRequirements(ETCDResource).
		AddDefaultEnvironment(e.fmtn, ss.Name()).
		AddEnvironmentVariable2("role", "etcd", true).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "INITIAL_CLUSTER_SIZE", Value: strconv.Itoa(etcdReplicas)},
			{Name: "SET_NAME", Value: ss.Name()},
			{Name: "RUNTIME_ENV", Value: strings.ToUpper(runtimeEnv)},
		}...).
		SetImage(e.config.Images[ETCDName]).
		SetSecurityContext(EtcdNonRootSecurityContext).
		ToContainer()
	c2.Lifecycle = ETCDlifecycle
	c2.ReadinessProbe = &ReadinessProbeEtcd
	c2.LivenessProbe = &LivenessProbeEtcd

	ss.AddInitContainer(c.ToContainer())
	ss.AddContainer(c2)
	ss.WaitForReady(false)
	return ss
}

func getETCDeP(etcdSts *app.StatefulSetBuilder, etcdSvc *plumbingresources.Service, etcdPorts []v1.ServicePort) string {
	etcdRepl := int(*etcdSts.StatefulSet().Spec.Replicas)
	etcdPort := int(etcdPorts[0].Port)
	etcdSvcName := etcdSvc.Name()
	podNamePrefix := etcdSts.StatefulSet().Name

	// Wolverine ETCD_ENDPOINT is expecting a tuple instead of service endpoint such as etcdSvc.Name()
	etcdEp := ""
	r, podNamePostFix := 3, "0"
	if etcdRepl > 1 {
		r = etcdRepl
	}
	for c := 0; c < r; c++ {
		if etcdRepl > 1 {
			podNamePostFix = strconv.Itoa(c)
		}
		etcdEp += "http://" + podNamePrefix + "-" + podNamePostFix + "." + etcdSvcName + ":" + strconv.Itoa(etcdPort) + ","
	}
	etcdEp = strings.TrimRight(etcdEp, ",")

	return etcdEp
}

func (e Db2uResourceCollection) getEtcdAffinity() v1.Affinity {
	var etcdAff v1.Affinity
	etcdAffinity := utils.GetValueWithDefaultValue(e.environment, "etcd.affinity", "")
	if etcdAffinity != "" {
		etcdAffinity = etcdAffinity.(map[string]interface{})
		jsonAffStr, err := json.Marshal(etcdAffinity)
		if err != nil {
			klog.Error("Unable to marshal etcd affinity", err.Error())
		}
		if err = json.Unmarshal(jsonAffStr, &etcdAff); err != nil {
			klog.Error("Unable to unmarshal etcd affinity", err.Error())
		}
	}
	return etcdAff
}

func (e Db2uResourceCollection) getEtcdTolerations() []v1.Toleration {
	var etcdTol []v1.Toleration
	etcdTolerations := utils.GetValueWithDefaultValue(e.environment, "etcd.tolerations", "")
	if etcdTolerations != "" {
		etcdTolerations = etcdTolerations.([]interface{})
		jsonTolStr, err := json.Marshal(etcdTolerations)
		if err != nil {
			klog.Error("Unable to marshal etcd tolerations", err.Error())
		}
		if err = json.Unmarshal(jsonTolStr, &etcdTol); err != nil {
			klog.Error("Unable to unmarshal etcd tolerations", err.Error())
		}
	}
	return etcdTol
}

func (e Db2uResourceCollection) skipAddAfinity(builder *app.PodBuilder) bool {
	if !e.isDb2uClusterV2() {
		return false
	} else {
		etcdSkipAffinity := utils.GetValueWithDefaultValue(e.environment, "etcd.ignoreDb2uAffinity", false).(bool)
		etcdAff := e.getEtcdAffinity()
		if builder.GetName() == ETCDName {
			if (etcdAff != v1.Affinity{}) || etcdSkipAffinity {
				return true
			}

		}
	}
	return false
}

// ---------------- Check if it's DV -------------------
func (e Db2uResourceCollection) isDvBase() bool {
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)
	return strings.ToLower(dbType) == "db2dv"
}

// ------------------ DV Utils Statefulset --------------------
func (e Db2uResourceCollection) newDVUtilsStatefulSet() app.StatefulSetBuilder {
	// Create the base DV Utils StateFulSet
	//Pass in Db2uRole, role just need to be something that exist in formation
	ss := app.NewStatefulSet(e.fmtn, DVUtilsName, DB2uRole)

	dvutilsReplicas := 1
	if e.isShutdown() {
		dvutilsReplicas = 0
	}

	ss.AddLabels("component", DVUtilsName)
	ss.WaitForReady(false)
	ss.SetReplicas(dvutilsReplicas)
	ss.SetServiceAccount(e.account.Name())
	ss.Spec.SecurityContext = RestrictedPodSecurityContext.DeepCopy()
	ss.Spec.AutomountServiceAccountToken = utils.PointerBool(false)

	c := app.NewContainer(DVUtilsName).
		// SetCommand([]string{"/scripts/dv-utils.sh"}).
		SetResourceRequirements(DVUtilsResource).
		AddDefaultEnvironment(e.fmtn, ss.Name()).
		AddEnvironmentVariable2("role", "dvutils", true).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "IS_KUBERNETES", Value: "true"},
		}...).
		SetImage(e.config.Images[DVUtilsName]).
		SetSecurityContext(DVUtilsContainerSecurityContext).
		ToContainer()
	c.Lifecycle = DVUtilslifecycle
	c.LivenessProbe = &LivenessProbeDVUtils
	c.ReadinessProbe = &ReadinessProbeDVUtils

	ss.AddContainer(c)
	ss.WaitForReady(false)
	return ss
}

// ----------------- DV Caching Deployment ------------------
func (e Db2uResourceCollection) newDVCaching() app.DeploymentBuilder {

	deployment := app.NewDeployment(e.fmtn, DVCachingName)
	deployment.SetRole(DVCachingName)
	deployment.WaitForReady(false)
	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = RestrictedPodSecurityContext.DeepCopy()
	deployment.Spec.AutomountServiceAccountToken = utils.PointerBool(false)
	//create the base tools deployment

	initCommand := "/opt/dv/current/caching/dv-caching-initcontainer.sh"

	cInit := app.NewContainer(fmt.Sprintf("%s-init", DVCachingName)).
		SetResourceRequirements(ToolsResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		SetImage(e.config.Images[DVCachingName]).
		SetSecurityContext(ToolsNonRootSecurityContext).
		SetCommand([]string{"/bin/sh", "-cx", initCommand}).
		ToContainer()

	c := app.NewContainer(DVCachingName).
		SetResourceRequirements(DVCachingResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable2("role", "dvcaching", true).
		SetImage(e.config.Images[DVCachingName]).
		SetSecurityContext(DVCachingContainerSecurityContext).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "METADB_SSL_PORT", Value: strconv.Itoa(int(Db2SslPort))},
			{Name: "METADB_USER", Value: "cacheadmin"},
			{Name: "METADB_PASSWORD_PATH", Value: "/secrets/cacheadminpwd/password"},
			{Name: "JDBC_CERTIFICATE_PATH", Value: "/secrets/db2ssl/tls.crt"},
			{Name: "JDBC_KEYSTORE_PATH", Value: "/tmp/jdbckeystore.jks"},
			{Name: "CACHING_KEYSTORE_PATH", Value: "/opt/dv/current/caching/jettycert/keystore.jks"},
			{Name: "METADB_DB_NAME", Value: "bigsql"},
			{Name: "THREADPOOL_SIZE", Value: "4"},
			{Name: "CACHESTORAGE_SIZE", Value: "100Gi"},
			{Name: "JWT_PUBLICKEY_URL", Value: JwtPublicKeyUrl},
			{Name: "AUTH_METHOD", Value: "JWT"},
			{Name: "SHUTDOWN_WAIT_TIME", Value: "30"},
		}...).ToContainer()

	c.LivenessProbe = LivenessProbeDVCaching
	c.ReadinessProbe = ReadinessProbeDVCaching

	deployment.AddInitContainer(cInit)
	deployment.AddContainer(c)
	return deployment
}

// ----------------- DV API Deployment ------------------
func (e Db2uResourceCollection) newDVApi() app.DeploymentBuilder {

	deployment := app.NewDeployment(e.fmtn, DVApiName)
	deployment.SetRole(DVApiName)
	deployment.WaitForReady(false)
	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = NonrootPodSecurityContext.DeepCopy()
	deployment.Spec.AutomountServiceAccountToken = utils.PointerBool(false)
	//create the base tools deployment

	c := app.NewContainer(DVApiName).
		SetResourceRequirements(DVApiResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable2("role", "dvapi", true).
		SetImage(e.config.Images[DVApiName]).
		SetSecurityContext(DVApiContainerSecurityContext).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "DV_DATABASE_SSL_ENABLE", Value: "true"},
			{Name: "DV_DATABASE_SSL_CERTFILE", Value: "/secrets/db2ssl/ca.crt"},
			{Name: "JWT", Value: "true"},
			{Name: "DV_POD_NAMESPACE", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "NGINX_SVC_HOST_NAME", Value: "https://internal-nginx-svc"},
			{Name: "NGINX_SVC_PORT", Value: "12443"},
			{Name: "JWT_URL_ENDPOINT", Value: "/auth/jwtpublic"},
		}...).ToContainer()

	c.LivenessProbe = LivenessProbeDVApi
	c.ReadinessProbe = ReadinessProbeDVApi

	deployment.AddContainer(c)
	return deployment
}

// ----------------- Hurricane ------------------
func (e Db2uResourceCollection) newHurricane() app.DeploymentBuilder {
	dbName := e.getDbName()
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "unknown").(string)
	deployment := app.NewDeployment(e.fmtn, HurricaneName)
	deployment.SetRole(HurricaneName)
	//Hurricane hold BigSQL I/O jars and Hadoop libs, in BigSQL mode we have to wait for this to be Ready or db2u will fail
	deployment.WaitForReady(true)
	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.SetServiceAccount(e.account.Name())
	//create the base tools deployment
	hurrImgName := HurricaneName
	customName := deployment.Name()
	if e.isDvBase() {
		customName = deployment.Name() + "-dv" // necessary for backward compatibility
		deployment.SetCustomName(customName)
		// TODO: remove fallback once db2u-watsonquery images are available in build
		if _, ok := e.config.Images[DB2WatsonQueryName]; !ok {
			// Fallback to "dv" hurricane image if db2u-watsonquery image isn't available - for DV sideload packages
			hurrImgName = HurricaneDvName
		}
	}
	c := app.NewContainer(HurricaneName).
		SetResourceRequirements(HurricaneResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable2("role", HurricaneName, true).
		SetImage(e.config.Images[hurrImgName]).
		SetSecurityContext(HurricaneSecurityContext).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "SCHEDULER_HOST", Value: customName},
			{Name: "DBNAME", Value: dbName},
			{Name: "DB2TYPE", Value: dbType},
			{Name: "MEMORY_REQUEST", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{Resource: "requests.memory"}}},
		}...).ToContainer()

	if e.isBigSQLBase() {
		c.LivenessProbe = LivenessProbeHurricane
	}
	c.ReadinessProbe = ReadinessProbeHurricane

	//Add support for read write once
	deployment.AddCustomSync(func(deployment *appsv1.Deployment) {
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	})
	deployment.AddContainer(c)

	_, rootPasswordOk := e.config.Secrets["MYSQL_ROOT_PASSWORD"]
	_, mysqlPasswordOk := e.config.Secrets["MYSQL_PASSWORD"]
	if rootPasswordOk && mysqlPasswordOk {
		c2 := app.NewContainer(SqlName).
			SetResourceRequirements(HurricaneSQLResource).
			AddDefaultEnvironment(e.fmtn, deployment.Name()).
			SetImage(e.config.Images[SqlName]).
			ToContainer()

		deployment.AddContainer(c2)
	}
	return deployment
}

// ----------------- Tools ------------------
func (e Db2uResourceCollection) newTools() app.DeploymentBuilder {
	//create the base tools deployment
	deployment := app.NewDeployment(e.fmtn, ToolsName)
	deployment.SetRole(ToolsName)
	deployment.WaitForReady(false)
	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = &ToolsPodSecurityContext
	c := app.NewContainer(ToolsName).
		SetResourceRequirements(ToolsResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable2("role", "tools", true).
		SetImage(e.config.Images[ToolsName]).
		SetSecurityContext(ToolsNonRootSecurityContext).
		AddPorts(true, v1.ContainerPort{Name: "dns", ContainerPort: int32(DnsTgtPort)}).
		ToContainer()
	c.ReadinessProbe = &ReadinessProbeTools
	c.LivenessProbe = &LivenessProbeTools

	deployment.AddContainer(c)
	deployment.WaitForReady(false)
	return deployment
}

// ---------------- LDAP -------------------
func (e Db2uResourceCollection) newLadp() app.DeploymentBuilder {
	deployment := app.NewDeployment(e.fmtn, LdapName)
	deployment.SetRole(LdapName)
	deployment.WaitForReady(false)
	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = &LdapPodSecurityContext

	c := app.NewContainer(LdapName).
		SetResourceRequirements(LdapResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable2("role", "LDAP", true).
		SetImage(e.config.Images[LdapName]).
		SetSecurityContext(LdapSecurityContext).
		ToContainer()

	//If set to rolling update, it fail to run ladp
	deployment.Deployment().Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	deployment.AddCustomSync(func(deployment *appsv1.Deployment) {
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	})
	c.ReadinessProbe = &ReadinessProbeLdap
	c.LivenessProbe = &LivenessProbeLdap

	deployment.AddContainer(c)
	return deployment
}

//-------------- Jobs ---------------

func (e Db2uResourceCollection) newInstdb() app.JobBuilder {

	_, isRestricted := e.checkAccount()

	job := app.NewJob(e.fmtn, SqllibSharedName)
	job.SetServiceAccount(e.account.Name())
	job.SetRestartPolicy(v1.RestartPolicyNever)
	job.WaitForCompleted(true)

	job.Spec.SecurityContext = NonrootPodSecurityContext.DeepCopy()
	instdbSC := InstdbSecurityContext
	if isRestricted != nil && *isRestricted {
		job.Spec.SecurityContext = RestrictedPodSecurityContext.DeepCopy()
		instdbSC = InstdbRestrictedSecurityContext
	}

	//job.Job().Spec.TTLSecondsAfterFinished = utils.PointerInt32(0)
	c := app.NewContainer(SqllibSharedName).
		SetResourceRequirements(SqllibSharedResource).
		SetCommand([]string{"/bin/sh", "-c", "/Db2wh_preinit/instdb_entrypoint.sh"}).
		AddDefaultEnvironment(e.fmtn, job.Name()).
		AddEnvironmentVariable2("role", SqllibSharedName, true).
		AddEnvironmentVariable2("SERVICE_NAME", utils.BaseName(e.fmtn), true).
		SetImage(e.config.Images[e.getInstdbImgName()]).
		SetSecurityContext(instdbSC).
		ToContainer()

	job.AddContainer(c)
	return job
}

func (e Db2uResourceCollection) newRestoreMorph(headNodeName string) app.JobBuilder {

	mainCommand := `source /tools/common.sh
	CAT_NODE=$(get_db2_head_node)
	kubectl exec -it -n ${NAMESPACE}  ${CAT_NODE?} -- bash -c "su - db2inst1 -c \"/db2u/db2u_restore_morph.sh\""
	exit $?`

	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)

	job := app.NewJob(e.fmtn, RestoreMorphName)
	job.SetServiceAccount(e.account.Name())
	job.SetRestartPolicy(v1.RestartPolicyNever)
	job.WaitForCompleted(true)
	job.SetBackoffLimit(2)
	//job.Job().Spec.TTLSecondsAfterFinished = utils.PointerInt32(0)
	job.Spec.SecurityContext = &NonrootPodSecurityContext

	numReplicas := e.config.Replicas
	if e.isSirius() {
		numReplicas = e.getTotalMemberSubsetNodeCount()
	}

	c := app.NewContainer(RestoreMorphName).
		SetResourceRequirements(RestoreMorphResource).
		SetCommand([]string{"/bin/bash", "-cx", mainCommand}).
		AddDefaultEnvironment(e.fmtn, job.Name()).
		AddEnvironmentVariable2("role", RestoreMorphName, true).
		AddEnvironmentVariable2("HEAD_NODE_NAME", headNodeName, true).
		AddEnvironmentVariable2("SERVICE_NAME", utils.BaseName(e.fmtn), true).
		SetImage(e.config.Images[ToolsName]).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "NAMESPACE", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "DB2TYPE", Value: dbType},
			{Name: "REPLICAS", Value: strconv.Itoa(int(numReplicas))},
			{Name: "APP", Value: e.fmtn.Spec.ID},
		}...).
		SetSecurityContext(NonRootSecurityContext)

	if utils.GetValueWithDefaultValue(e.advOpts, "dedicatedCatalogNode", false).(bool) {
		c.AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "IS_USING_DEDICATED_CATALOG_NODE", Value: "true"},
		}...)
	}

	//Reuse the same container as the main one but overwrite the command
	cInit := c.ToContainer().DeepCopy()
	cInit.Name = RestoreMorphName + "-init"
	cInit.Command = []string{
		"/bin/bash",
		"-cx",
		"/tools/post-install/db2u_ready.sh --replicas ${REPLICAS} --template ${APP} --namespace ${NAMESPACE} --dbType ${DB2TYPE}"}

	job.AddContainer(c.ToContainer())
	job.AddInitContainer(cInit)
	return job
}

//-------Upgrade-------------
//TODO Move main container scripts into the images

//TODO We don't use configmap/hadr at the moment, is HADR CRD going to update the configmap?
// Should the flow following something like: HADR CRD -> Update Db2uCluster CRD -> foramtions -> update configmap

// This job get deploy after restoreMorph, we wait for restoreMorph to finish before moving on to the next resource.
func (e Db2uResourceCollection) NewDb2uUpdateJobs(headNodeName string) app.JobBuilder {
	_, isRestricted := e.checkAccount()

	sudoCmd := "sudo "
	if isRestricted != nil && *isRestricted {
		sudoCmd = ""
	}

	initCommand := `source /tools/common.sh
/tools/post-install/upgrade_init.sh --namespace ${NAMESPACE} --dbType ${DB2TYPE} --appName ${APP}`

	initVrmfCommand := fmt.Sprintf(`source /tools/common.sh
# Verify all Db2U PODs are ready before running VRMF check
/tools/post-install/db2u_ready.sh --namespace ${NAMESPACE} --replicas ${REPLICAS} --dbType ${DB2TYPE} --template ${APP}

CAT_NODE=$(get_db2_head_node)
kubectl exec -it -n ${NAMESPACE} ${CAT_NODE?} -- bash -c "%s/db2u/scripts/detect_db2_vrmf_change.sh -file"`, sudoCmd)

	initExtractDb2ImageCommand := `tar xvf /db2_nonroot_image/* -C /mnt/blumeta0/home/db2inst1/sqllib/`

	// TODO: evaluate NonrootPodSecurityContext - user 500 necessary for extraction?
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)

	//TODO Jobs name will be coming from formations, If db2uCluster see that the version change.

	// The name of this job will reflect that. Ex: db2u-update-11.5.1.0-to-11.5.2.1
	podName, ok := e.config.Env["upgrade/podname"]
	if _, ok2 := e.fmtn.Annotations["upgrade/force"]; !ok2 {
		if !ok || e.isBigSQLBase() {
			return app.JobBuilder{}
		}
	}
	job := app.NewJob(e.fmtn, podName)
	job.SetServiceAccount(e.account.Name())
	job.SetRestartPolicy(v1.RestartPolicyNever)
	job.WaitForCompleted(true)
	job.SetBackoffLimit(2)
	job.Spec.SecurityContext = &NonrootPodSecurityContext
	//job.Job().Spec.TTLSecondsAfterFinished = utils.PointerInt32(0)
	c := app.NewContainer(UpgradeName).
		SetResourceRequirements(UpgradeResource).
		SetCommand([]string{"/bin/bash", "-cx", UpgradeScriptCommand}).
		AddDefaultEnvironment(e.fmtn, job.Name()).
		SetImage(e.config.Images[ToolsName]).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "NAMESPACE", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "DB2TYPE", Value: dbType},
			{Name: "REPLICAS", Value: strconv.Itoa(int(e.config.Replicas))},
			{Name: "APP", Value: e.fmtn.Spec.ID},
			{Name: "HEAD_NODE_NAME", Value: headNodeName},
		}...).
		SetSecurityContext(NonRootSecurityContext).
		ToContainer()

	//Reuse container and replace what is needed
	initC := c.DeepCopy()
	initC.Command = []string{"/bin/bash", "-cx", initCommand}
	initC.Name = UpgradeInitOneName
	initC.Resources = UpgradeResourceInit1

	initVrmfC := c.DeepCopy()
	initVrmfC.Command = []string{"/bin/bash", "-cx", initVrmfCommand}
	initVrmfC.Name = UpgradeInitTwoName
	initVrmfC.Resources = UpgradeResourceInit1

	job.AddInitContainer(initC)
	job.AddInitContainer(initVrmfC)

	if isRestricted != nil && *isRestricted {
		initExtractDb2Image := c.DeepCopy()
		initExtractDb2Image.Command = []string{"/bin/bash", "-cx", initExtractDb2ImageCommand}
		initExtractDb2Image.Name = UpgradeInitThreeName
		initExtractDb2Image.Resources = UpgradeResourceInit1
		initExtractDb2Image.Image = e.config.Images[RestrictedUpdateImage]

		job.AddInitContainer(initExtractDb2Image)
	}

	job.AddContainer(c)
	return job
}

// ---------------- REST -------------------
func (e Db2uResourceCollection) newRest(serviceName string) app.DeploymentBuilder {
	deployment := app.NewDeployment(e.fmtn, RestName)
	deployment.SetRole(RestName)
	deployment.WaitForReady(false)
	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = &RestPodSecurityContext
	dbName := e.getDbName()
	sslConnectEnabled := utils.GetValueWithDefaultValue(e.addOns, "rest.useSslForDbConnect", true).(bool)
	dbConnectPort := Db2Port
	if sslConnectEnabled {
		dbConnectPort = Db2SslPort
	}
	c := app.NewContainer(RestName).
		SetResourceRequirements(RestResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "DB2REST_DBHOSTNAME", Value: serviceName},
			{Name: "DB2REST_DBPORT", Value: strconv.Itoa(int(dbConnectPort))},
			{Name: "DB2REST_SSLCONNECTION", Value: strconv.FormatBool(sslConnectEnabled)},
			{Name: "DB2REST_DBNAME", Value: dbName},
		}...).
		SetImage(e.config.Images[RestName]).
		SetSecurityContext(RestSecurityContext).
		ToContainer()

	//If set to rolling update, it fail to run rest
	deployment.Deployment().Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	deployment.AddCustomSync(func(deployment *appsv1.Deployment) {
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	})
	c.ReadinessProbe = &ReadinessProbeRest
	c.LivenessProbe = &LivenessProbeRest

	deployment.AddContainer(c)
	return deployment
}

// ---------------- GRAPH -------------------
func (e Db2uResourceCollection) newGraph(serviceName string) app.DeploymentBuilder {
	deployment := app.NewDeployment(e.fmtn, GraphName)
	deployment.SetRole(GraphName)
	deployment.WaitForReady(false)
	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = &GraphPodSecurityContext
	dbName := e.getDbName()
	sslConnectEnabled := utils.GetValueWithDefaultValue(e.addOns, "graph.useSslForDbConnect", true).(bool)
	dbConnectPort := Db2Port
	if sslConnectEnabled {
		dbConnectPort = Db2SslPort
	}

	c := app.NewContainer(GraphName).
		SetResourceRequirements(GraphResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "DB2GRAPH_DBHOST", Value: serviceName},
			{Name: "DB2GRAPH_DBPORT", Value: strconv.Itoa(int(dbConnectPort))},
			{Name: "DB2GRAPH_IS_SSL", Value: strconv.FormatBool(sslConnectEnabled)},
			{Name: "DB2GRAPH_DBNAME", Value: dbName},
		}...).
		SetImage(e.config.Images[GraphName]).
		SetSecurityContext(GraphSecurityContext).
		ToContainer()

	//If set to rolling update, it fail to run graph
	deployment.Deployment().Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	deployment.AddCustomSync(func(deployment *appsv1.Deployment) {
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	})
	c.ReadinessProbe = &ReadinessProbeGraph
	c.LivenessProbe = &LivenessProbeGraph

	deployment.AddContainer(c)
	return deployment
}

func (e Db2uResourceCollection) newCatalog(apiEnvVar []v1.EnvVar, apiEnvVarCert []v1.EnvVar, toolsSvcName string) (app.DeploymentBuilder, plumbingresources.Service) {
	db2uCatalogLabel := labels.New()
	catalogSvc := plumbingresources.NewClusterIPService(
		e.fmtn,
		"db2u-catalog", "",
		[]v1.ServicePort{
			{Name: "main", Protocol: v1.ProtocolTCP, Port: Db2Port},
			{Name: "wvha-rest", Protocol: v1.ProtocolTCP, Port: WvHaRestPort},
			{Name: "db2uapi", Protocol: v1.ProtocolTCP, Port: Db2uapi},
			{Name: "ssh-port", Protocol: v1.ProtocolTCP, Port: Db2uSshPort},
		}, db2uCatalogLabel)

	deployment := app.NewDeployment(e.fmtn, CatalogName)
	dbType := utils.GetValueWithDefaultValue(e.environment, "dbType", "db2wh").(string)

	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.AddLabels("type", "catalog").
		AddLabels("component", dbType).
		SetRole(CatalogName)
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = NonrootPodSecurityContext.DeepCopy()

	// If privileged, then we can set IPC Kernel params via the init Container
	// If spec.account or spec.account.privileged is not specified at deployment time
	initCmdDb2Kernel := initCommandDB2Kernel
	initKernelSC := InitKernelSecurityContext

	_, isRestricted := e.checkAccount()

	c := e.newDb2InitKernelContainer(CatalogNameInitKernel, CatalogName, deployment.Name(), initCmdDb2Kernel, initKernelSC)

	imageName := DB2SiriusName
	db2SecurityContext := Db2SecurityContext
	if isRestricted != nil && *isRestricted {
		imageName = DB2RestrictedName
		db2SecurityContext = Db2RestrictedSecurityContext
	}

	c3 := e.newDb2EngineContainer(CatalogName, Db2uSiriusResource, db2SecurityContext, imageName, isRestricted)
	c3.AddEnvironmentVariable(true, []v1.EnvVar{
		{Name: "IS_DEDICATED_CATALOG_NODE", Value: "true"},
	}...)

	deployment.Spec.DNSConfig = &v1.PodDNSConfig{Options: []v1.PodDNSConfigOption{{Name: "ndots", Value: utils.PointerString("2")}}}
	deployment.Spec.HostIPC = true
	deployment.AddInitContainer(c.ToContainer())
	deployment.AddContainer(c3.ToContainer())
	deployment.WaitForReady(true)

	catSvcEnvVar := []v1.EnvVar{
		{Name: "CATALOG_SERVICE_NAME", Value: catalogSvc.Name()},
	}
	deployment.AddEnvToAllContainer(apiEnvVar...)
	deployment.AddEnvToAllContainer(apiEnvVarCert...)
	deployment.AddEnvToAllContainer(catSvcEnvVar...)

	nameServerVar := strings.ToUpper(strings.Replace(toolsSvcName, "-", "_", -1))
	deployment.AddEnvToContainer(CatalogName, v1.EnvVar{Name: "NAME_SERVER_VARIABLE", Value: nameServerVar + "_SERVICE_HOST"})
	db2uCatalogLabel.AddMany(deployment.Labels().Map())

	return deployment, catalogSvc
}

// Return cluster infrastructure node name and IP
func (e Db2uResourceCollection) getInfraHost() (string, string) {
	infraHost := fmt.Sprint(utils.GetValueWithDefaultValue(e.addOns, "qrep.infraHost", "unknown"))
	infraIP := fmt.Sprint(utils.GetValueWithDefaultValue(e.addOns, "qrep.infraIP", "unknown"))
	return infraHost, infraIP
}

func (e Db2uResourceCollection) getMemberSubsets() []MemberSubset {
	memberSubsets := []MemberSubset{}
	if e.advOpts != nil {
		if memberSubsetsIf, ok := e.advOpts["memberSubsets"]; ok {
			tmpArray, ok := memberSubsetsIf.([]interface{})
			if !ok {
				return memberSubsets
			}
			for _, element := range tmpArray {
				eMap, ok := element.(map[string]interface{})
				if !ok {
					return memberSubsets
				}
				memberSubset := MemberSubset{}
				if theName, ok := eMap["name"]; ok {
					memberSubset.Name = theName.(string)
				}
				if theDatabaseAlias, ok := eMap["databaseAlias"]; ok {
					memberSubset.DatabaseAlias = theDatabaseAlias.(string)
				}
				if theType, ok := eMap["type"]; ok {
					memberSubset.Type = theType.(string)
				}
				if theSize, ok := eMap["size"]; ok {
					memberSubset.Size = int32(theSize.(float64))
				}
				memberSubsets = append(memberSubsets, memberSubset)
			}
		}
	}
	return memberSubsets
}

// ---------------- QRep -------------------
func (e Db2uResourceCollection) newQrep(serviceName string) app.DeploymentBuilder {
	deployment := app.NewDeployment(e.fmtn, QrepName)
	deployment.SetRole(QrepName)
	deployment.WaitForReady(false)
	deployment.SetReplicas(1)
	if e.isShutdown() {
		deployment.SetReplicas(0)
	}
	deployment.SetServiceAccount(e.account.Name())
	deployment.Spec.SecurityContext = &QrepPodSecurityContext
	dbName := e.getDbName()

	var infraHost, infraIP string
	infraHost, infraIP = e.getInfraHost()

	sslConnectEnabled := utils.GetValueWithDefaultValue(e.addOns, "qrep.useSslForDbConnect", true).(bool)
	dbConnectPort := Db2Port
	if sslConnectEnabled {
		dbConnectPort = Db2SslPort
	}

	c := app.NewContainer(QrepName).
		SetResourceRequirements(QrepResource).
		AddDefaultEnvironment(e.fmtn, deployment.Name()).
		AddEnvironmentVariable(true, []v1.EnvVar{
			{Name: "DB2QREP_DBHOST", Value: serviceName},
			{Name: "DB2QREP_DBPORT", Value: strconv.Itoa(int(dbConnectPort))},
			{Name: "DB2QREP_IS_SSL", Value: strconv.FormatBool(sslConnectEnabled)},
			{Name: "DB2QREP_DBNAME", Value: dbName},
			{Name: "INFRA_HOST", Value: infraHost},
			{Name: "INFRA_IP", Value: infraIP},
		}...).
		SetImage(e.config.Images[QrepName]).
		SetSecurityContext(QrepSecurityContext).
		ToContainer()

	deployment.Deployment().Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	deployment.AddCustomSync(func(deployment *appsv1.Deployment) {
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	})

	//Todo - Enable readiness and liveness probes later
	c.ReadinessProbe = &ReadinessProbeQrep
	c.LivenessProbe = &LivenessProbeQrep

	deployment.AddContainer(c)
	return deployment
}

func (e Db2uResourceCollection) getDbName() string {
	var dbName string
	if e.isDb2uClusterV2() {
		dbName = utils.GetValueWithDefaultValue(e.databases[0], "name", "bludb").(string)
	} else {
		dbName = utils.GetValueWithDefaultValue(e.environment, "database.name", "bludb").(string)
	}
	return dbName
}
