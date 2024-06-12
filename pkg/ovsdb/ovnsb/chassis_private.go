// Code generated by "libovsdb.modelgen"
// DO NOT EDIT.

package ovnsb

const ChassisPrivateTable = "Chassis_Private"

// ChassisPrivate defines an object in Chassis_Private table
type ChassisPrivate struct {
	UUID           string            `ovsdb:"_uuid"`
	Chassis        *string           `ovsdb:"chassis"`
	ExternalIDs    map[string]string `ovsdb:"external_ids"`
	Name           string            `ovsdb:"name"`
	NbCfg          int               `ovsdb:"nb_cfg"`
	NbCfgTimestamp int               `ovsdb:"nb_cfg_timestamp"`
}