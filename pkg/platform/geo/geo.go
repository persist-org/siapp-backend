// Package geo holds the small geographic value types SiAPP shares between modules.
package geo

// Point is a place on the earth in decimal degrees (WGS 84): the same unit the database
// stores and the apps send.
type Point struct {
	Lat float64
	Lng float64
}
