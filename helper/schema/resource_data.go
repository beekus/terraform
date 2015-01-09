package schema

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/terraform/terraform"
	"github.com/mitchellh/mapstructure"
)

// ResourceData is used to query and set the attributes of a resource.
//
// ResourceData is the primary argument received for CRUD operations on
// a resource as well as configuration of a provider. It is a powerful
// structure that can be used to not only query data, but check for changes,
// define partial state updates, etc.
//
// The most relevant methods to take a look at are Get, Set, and Partial.
type ResourceData struct {
	// Settable (internally)
	schema  map[string]*Schema
	config  *terraform.ResourceConfig
	state   *terraform.InstanceState
	diff    *terraform.InstanceDiff
	diffing bool

	// Don't set
	multiReader *MultiLevelFieldReader
	setMap      map[string]string
	newState    *terraform.InstanceState
	partial     bool
	partialMap  map[string]struct{}
	once        sync.Once
}

// getSource represents the level we want to get for a value (internally).
// Any source less than or equal to the level will be loaded (whichever
// has a value first).
type getSource byte

const (
	getSourceState getSource = 1 << iota
	getSourceConfig
	getSourceSet
	getSourceExact               // Only get from the _exact_ level
	getSourceDiff                // Apply the diff on top our level
	getSourceLevelMask getSource = getSourceState | getSourceConfig | getSourceSet
	getSourceMax       getSource = getSourceSet
)

// getResult is the internal structure that is generated when a Get
// is called that contains some extra data that might be used.
type getResult struct {
	Value          interface{}
	ValueProcessed interface{}
	Computed       bool
	Exists         bool
	Schema         *Schema
}

var getResultEmpty getResult

// Get returns the data for the given key, or nil if the key doesn't exist
// in the schema.
//
// If the key does exist in the schema but doesn't exist in the configuration,
// then the default value for that type will be returned. For strings, this is
// "", for numbers it is 0, etc.
//
// If you want to test if something is set at all in the configuration,
// use GetOk.
func (d *ResourceData) Get(key string) interface{} {
	v, _ := d.GetOk(key)
	return v
}

// GetChange returns the old and new value for a given key.
//
// HasChange should be used to check if a change exists. It is possible
// that both the old and new value are the same if the old value was not
// set and the new value is. This is common, for example, for boolean
// fields which have a zero value of false.
func (d *ResourceData) GetChange(key string) (interface{}, interface{}) {
	o, n := d.getChange(key, getSourceConfig, getSourceConfig|getSourceDiff)
	return o.Value, n.Value
}

// GetOk returns the data for the given key and whether or not the key
// existed or not in the configuration. The second boolean result will also
// be false if a key is given that isn't in the schema at all.
//
// The first result will not necessarilly be nil if the value doesn't exist.
// The second result should be checked to determine this information.
func (d *ResourceData) GetOk(key string) (interface{}, bool) {
	r := d.getRaw(key, getSourceSet|getSourceDiff)
	return r.Value, r.Exists
}

func (d *ResourceData) getRaw(key string, level getSource) getResult {
	var parts []string
	if key != "" {
		parts = strings.Split(key, ".")
	}

	schema := &Schema{Type: typeObject, Elem: d.schema}
	return d.get("", parts, schema, level)
}

// HasChange returns whether or not the given key has been changed.
func (d *ResourceData) HasChange(key string) bool {
	o, n := d.GetChange(key)
	return !reflect.DeepEqual(o, n)
}

// hasComputedSubKeys walks through a schema and returns whether or not the
// given key contains any subkeys that are computed.
func (d *ResourceData) hasComputedSubKeys(key string, schema *Schema) bool {
	prefix := key + "."

	switch t := schema.Elem.(type) {
	case *Resource:
		for k, schema := range t.Schema {
			if d.config.IsComputed(prefix + k) {
				return true
			}
			if d.hasComputedSubKeys(prefix+k, schema) {
				return true
			}
		}
	}
	return false
}

// Partial turns partial state mode on/off.
//
// When partial state mode is enabled, then only key prefixes specified
// by SetPartial will be in the final state. This allows providers to return
// partial states for partially applied resources (when errors occur).
func (d *ResourceData) Partial(on bool) {
	d.partial = on
	if on {
		if d.partialMap == nil {
			d.partialMap = make(map[string]struct{})
		}
	} else {
		d.partialMap = nil
	}
}

// Set sets the value for the given key.
//
// If the key is invalid or the value is not a correct type, an error
// will be returned.
func (d *ResourceData) Set(key string, value interface{}) error {
	d.once.Do(d.init)
	parts := strings.Split(key, ".")
	return d.setObject("", parts, d.schema, value)
}

// SetPartial adds the key prefix to the final state output while
// in partial state mode.
//
// If partial state mode is disabled, then this has no effect. Additionally,
// whenever partial state mode is toggled, the partial data is cleared.
func (d *ResourceData) SetPartial(k string) {
	if d.partial {
		d.partialMap[k] = struct{}{}
	}
}

// Id returns the ID of the resource.
func (d *ResourceData) Id() string {
	var result string

	if d.state != nil {
		result = d.state.ID
	}

	if d.newState != nil {
		result = d.newState.ID
	}

	return result
}

// ConnInfo returns the connection info for this resource.
func (d *ResourceData) ConnInfo() map[string]string {
	if d.newState != nil {
		return d.newState.Ephemeral.ConnInfo
	}

	if d.state != nil {
		return d.state.Ephemeral.ConnInfo
	}

	return nil
}

// SetId sets the ID of the resource. If the value is blank, then the
// resource is destroyed.
func (d *ResourceData) SetId(v string) {
	d.once.Do(d.init)
	d.newState.ID = v
}

// SetConnInfo sets the connection info for a resource.
func (d *ResourceData) SetConnInfo(v map[string]string) {
	d.once.Do(d.init)
	d.newState.Ephemeral.ConnInfo = v
}

// State returns the new InstanceState after the diff and any Set
// calls.
func (d *ResourceData) State() *terraform.InstanceState {
	var result terraform.InstanceState
	result.ID = d.Id()

	// If we have no ID, then this resource doesn't exist and we just
	// return nil.
	if result.ID == "" {
		return nil
	}

	result.Attributes = d.stateObject("", d.schema)
	result.Ephemeral.ConnInfo = d.ConnInfo()

	if v := d.Id(); v != "" {
		result.Attributes["id"] = d.Id()
	}

	return &result
}

func (d *ResourceData) init() {
	// Initialize the field that will store our new state
	var copyState terraform.InstanceState
	if d.state != nil {
		copyState = *d.state
	}
	d.newState = &copyState

	// Initialize the map for storing set data
	d.setMap = make(map[string]string)

	// Initialize the reader for getting data from the
	// underlying sources (config, diff, etc.)
	readers := make(map[string]FieldReader)
	var stateAttributes map[string]string
	if d.state != nil {
		stateAttributes = d.state.Attributes
		readers["state"] = &MapFieldReader{
			Schema: d.schema,
			Map:    BasicMapReader(stateAttributes),
		}
	}
	if d.config != nil {
		readers["config"] = &ConfigFieldReader{
			Schema: d.schema,
			Config: d.config,
		}
	}
	if d.diff != nil {
		readers["diff"] = &DiffFieldReader{
			Schema: d.schema,
			Diff:   d.diff,
			Source: &MultiLevelFieldReader{
				Levels:  []string{"state", "config"},
				Readers: readers,
			},
		}
	}
	readers["set"] = &MapFieldReader{
		Schema: d.schema,
		Map:    BasicMapReader(d.setMap),
	}
	d.multiReader = &MultiLevelFieldReader{
		Levels: []string{
			"state",
			"config",
			"diff",
			"set",
		},

		Readers: readers,
	}
}

func (d *ResourceData) diffChange(
	k string) (interface{}, interface{}, bool, bool) {
	// Get the change between the state and the config.
	o, n := d.getChange(k, getSourceState, getSourceConfig|getSourceExact)
	if !o.Exists {
		o.Value = nil
	}
	if !n.Exists {
		n.Value = nil
	}

	// Return the old, new, and whether there is a change
	return o.Value, n.Value, !reflect.DeepEqual(o.Value, n.Value), n.Computed
}

func (d *ResourceData) getChange(
	key string,
	oldLevel getSource,
	newLevel getSource) (getResult, getResult) {
	var parts, parts2 []string
	if key != "" {
		parts = strings.Split(key, ".")
		parts2 = strings.Split(key, ".")
	}

	schema := &Schema{Type: typeObject, Elem: d.schema}
	o := d.get("", parts, schema, oldLevel)
	n := d.get("", parts2, schema, newLevel)
	return o, n
}

func (d *ResourceData) get(
	k string,
	parts []string,
	schema *Schema,
	source getSource) getResult {
	d.once.Do(d.init)

	level := "set"
	flags := source & ^getSourceLevelMask
	diff := flags&getSourceDiff != 0
	exact := flags&getSourceExact != 0
	source = source & getSourceLevelMask
	if source >= getSourceSet {
		level = "set"
	} else if diff {
		level = "diff"
	} else if source >= getSourceConfig {
		level = "config"
	} else {
		level = "state"
	}

	// Build the address of the key we're looking for and ask the FieldReader
	var addr []string
	if k != "" {
		addr = strings.Split(k, ".")
	}
	addr = append(addr, parts...)
	for i, v := range addr {
		if v[0] == '~' {
			addr[i] = v[1:]
		}
	}

	var result FieldReadResult
	var err error
	if exact {
		result, err = d.multiReader.ReadFieldExact(addr, level)
	} else {
		result, err = d.multiReader.ReadFieldMerge(addr, level)
	}
	if err != nil {
		panic(err)
	}

	// If the result doesn't exist, then we set the value to the zero value
	if result.Value == nil {
		if schemaL := addrToSchema(addr, d.schema); len(schemaL) > 0 {
			schema := schemaL[len(schemaL)-1]
			result.Value = result.ValueOrZero(schema)
		}
	}

	// Transform the FieldReadResult into a getResult. It might be worth
	// merging these two structures one day.
	return getResult{
		Value:          result.Value,
		ValueProcessed: result.ValueProcessed,
		Computed:       result.Computed,
		Exists:         result.Exists,
		Schema:         schema,
	}
}

func (d *ResourceData) set(
	k string,
	parts []string,
	schema *Schema,
	value interface{}) error {
	switch schema.Type {
	case TypeList:
		return d.setList(k, parts, schema, value)
	case TypeMap:
		return d.setMapValue(k, parts, schema, value)
	case TypeSet:
		return d.setSet(k, parts, schema, value)
	case TypeBool:
		fallthrough
	case TypeInt:
		fallthrough
	case TypeString:
		return d.setPrimitive(k, schema, value)
	default:
		panic(fmt.Sprintf("%s: unknown type %#v", k, schema.Type))
	}
}

func (d *ResourceData) setList(
	k string,
	parts []string,
	schema *Schema,
	value interface{}) error {
	if len(parts) > 0 {
		return fmt.Errorf("%s: can only set the full list, not elements", k)
	}

	setElement := func(k string, idx string, value interface{}) error {
		if idx == "#" {
			return fmt.Errorf("%s: can't set count of list", k)
		}

		key := fmt.Sprintf("%s.%s", k, idx)
		switch t := schema.Elem.(type) {
		case *Resource:
			return d.setObject(key, nil, t.Schema, value)
		case *Schema:
			return d.set(key, nil, t, value)
		}

		return nil
	}

	var vs []interface{}
	if err := mapstructure.Decode(value, &vs); err != nil {
		return fmt.Errorf("%s: %s", k, err)
	}

	// Set the entire list.
	var err error
	for i, elem := range vs {
		is := strconv.FormatInt(int64(i), 10)
		err = setElement(k, is, elem)
		if err != nil {
			break
		}
	}
	if err != nil {
		for i, _ := range vs {
			is := strconv.FormatInt(int64(i), 10)
			setElement(k, is, nil)
		}

		return err
	}

	d.setMap[k+".#"] = strconv.FormatInt(int64(len(vs)), 10)
	return nil
}

func (d *ResourceData) setMapValue(
	k string,
	parts []string,
	schema *Schema,
	value interface{}) error {
	elemSchema := &Schema{Type: TypeString}
	if len(parts) > 0 {
		return fmt.Errorf("%s: full map must be set, no a single element", k)
	}

	v := reflect.ValueOf(value)
	if v.Kind() != reflect.Map {
		return fmt.Errorf("%s: must be a map", k)
	}
	if v.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("%s: keys must strings", k)
	}
	vs := make(map[string]interface{})
	for _, mk := range v.MapKeys() {
		mv := v.MapIndex(mk)
		vs[mk.String()] = mv.Interface()
	}

	if len(vs) == 0 {
		// The empty string here means the map is removed.
		d.setMap[k] = ""
		return nil
	}

	delete(d.setMap, k)
	for subKey, v := range vs {
		err := d.set(fmt.Sprintf("%s.%s", k, subKey), nil, elemSchema, v)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *ResourceData) setObject(
	k string,
	parts []string,
	schema map[string]*Schema,
	value interface{}) error {
	if len(parts) > 0 {
		// We're setting a specific key in an object
		key := parts[0]
		parts = parts[1:]

		s, ok := schema[key]
		if !ok {
			return fmt.Errorf("%s (internal): unknown key to set: %s", k, key)
		}

		if k != "" {
			// If we're not at the root, then we need to append
			// the key to get the full key path.
			key = fmt.Sprintf("%s.%s", k, key)
		}

		return d.set(key, parts, s, value)
	}

	// Set the entire object. First decode into a proper structure
	var v map[string]interface{}
	if err := mapstructure.Decode(value, &v); err != nil {
		return fmt.Errorf("%s: %s", k, err)
	}

	// Set each element in turn
	var err error
	for k1, v1 := range v {
		err = d.setObject(k, []string{k1}, schema, v1)
		if err != nil {
			break
		}
	}
	if err != nil {
		for k1, _ := range v {
			d.setObject(k, []string{k1}, schema, nil)
		}
	}

	return err
}

func (d *ResourceData) setPrimitive(
	k string,
	schema *Schema,
	v interface{}) error {
	if v == nil {
		delete(d.setMap, k)
		return nil
	}

	var set string
	switch schema.Type {
	case TypeBool:
		var b bool
		if err := mapstructure.Decode(v, &b); err != nil {
			return fmt.Errorf("%s: %s", k, err)
		}

		set = strconv.FormatBool(b)
	case TypeString:
		if err := mapstructure.Decode(v, &set); err != nil {
			return fmt.Errorf("%s: %s", k, err)
		}
	case TypeInt:
		var n int
		if err := mapstructure.Decode(v, &n); err != nil {
			return fmt.Errorf("%s: %s", k, err)
		}

		set = strconv.FormatInt(int64(n), 10)
	default:
		return fmt.Errorf("Unknown type: %#v", schema.Type)
	}

	d.setMap[k] = set
	return nil
}

func (d *ResourceData) setSet(
	k string,
	parts []string,
	schema *Schema,
	value interface{}) error {
	if len(parts) > 0 {
		return fmt.Errorf("%s: can only set the full set, not elements", k)
	}

	// If it is a slice, then we have to turn it into a *Set so that
	// we get the proper order back based on the hash code.
	if v := reflect.ValueOf(value); v.Kind() == reflect.Slice {
		// Build a temp *ResourceData to use for the conversion
		tempD := &ResourceData{
			setMap: make(map[string]string),
			schema: map[string]*Schema{k: schema},
		}
		tempD.once.Do(tempD.init)

		// Set the entire list, this lets us get sane values out of it
		if err := tempD.setList(k, nil, schema, value); err != nil {
			return err
		}

		// Build the set by going over the list items in order and
		// hashing them into the set. The reason we go over the list and
		// not the `value` directly is because this forces all types
		// to become []interface{} (generic) instead of []string, which
		// most hash functions are expecting.
		s := &Set{F: schema.Set}
		source := getSourceSet | getSourceExact
		for i := 0; i < v.Len(); i++ {
			is := strconv.FormatInt(int64(i), 10)
			result := tempD.get(k, []string{is}, schema, source)
			if !result.Exists {
				panic("just set item doesn't exist")
			}

			s.Add(result.Value)
		}

		value = s
	}

	switch t := schema.Elem.(type) {
	case *Resource:
		for code, elem := range value.(*Set).m {
			for field, _ := range t.Schema {
				subK := fmt.Sprintf("%s.%d", k, code)
				value := elem.(map[string]interface{})[field]
				err := d.setObject(subK, []string{field}, t.Schema, value)
				if err != nil {
					return err
				}
			}
		}
	case *Schema:
		for code, elem := range value.(*Set).m {
			subK := fmt.Sprintf("%s.%d", k, code)
			err := d.set(subK, nil, t, elem)
			if err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s: unknown element type (internal)", k)
	}

	d.setMap[k+".#"] = strconv.Itoa(value.(*Set).Len())
	return nil
}

func (d *ResourceData) stateList(
	prefix string,
	schema *Schema) map[string]string {
	countRaw := d.get(prefix, []string{"#"}, schema, d.stateSource(prefix))
	if !countRaw.Exists {
		if schema.Computed {
			// If it is computed, then it always _exists_ in the state,
			// it is just empty.
			countRaw.Exists = true
			countRaw.Value = 0
		} else {
			return nil
		}
	}
	count := countRaw.Value.(int)

	result := make(map[string]string)
	if count > 0 || schema.Computed {
		result[prefix+".#"] = strconv.FormatInt(int64(count), 10)
	}
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("%s.%d", prefix, i)

		var m map[string]string
		switch t := schema.Elem.(type) {
		case *Resource:
			m = d.stateObject(key, t.Schema)
		case *Schema:
			m = d.stateSingle(key, t)
		}

		for k, v := range m {
			result[k] = v
		}
	}

	return result
}

func (d *ResourceData) stateMap(
	prefix string,
	schema *Schema) map[string]string {
	v := d.get(prefix, nil, schema, d.stateSource(prefix))
	if !v.Exists {
		return nil
	}

	elemSchema := &Schema{Type: TypeString}
	result := make(map[string]string)
	for mk, _ := range v.Value.(map[string]interface{}) {
		mp := fmt.Sprintf("%s.%s", prefix, mk)
		for k, v := range d.stateSingle(mp, elemSchema) {
			result[k] = v
		}
	}

	return result
}

func (d *ResourceData) stateObject(
	prefix string,
	schema map[string]*Schema) map[string]string {
	result := make(map[string]string)
	for k, v := range schema {
		key := k
		if prefix != "" {
			key = prefix + "." + key
		}

		for k1, v1 := range d.stateSingle(key, v) {
			result[k1] = v1
		}
	}

	return result
}

func (d *ResourceData) statePrimitive(
	prefix string,
	schema *Schema) map[string]string {
	raw := d.getRaw(prefix, d.stateSource(prefix))
	if !raw.Exists {
		return nil
	}

	v := raw.Value
	if raw.ValueProcessed != nil {
		v = raw.ValueProcessed
	}

	var vs string
	switch schema.Type {
	case TypeBool:
		vs = strconv.FormatBool(v.(bool))
	case TypeString:
		vs = v.(string)
	case TypeInt:
		vs = strconv.FormatInt(int64(v.(int)), 10)
	default:
		panic(fmt.Sprintf("Unknown type: %#v", schema.Type))
	}

	return map[string]string{
		prefix: vs,
	}
}

func (d *ResourceData) stateSet(
	prefix string,
	schema *Schema) map[string]string {
	raw := d.get(prefix, nil, schema, d.stateSource(prefix))
	if !raw.Exists {
		if schema.Computed {
			// If it is computed, then it always _exists_ in the state,
			// it is just empty.
			raw.Exists = true
			raw.Value = new(Set)
		} else {
			return nil
		}
	}

	set := raw.Value.(*Set)
	result := make(map[string]string)
	result[prefix+".#"] = strconv.Itoa(set.Len())

	for _, idx := range set.listCode() {
		key := fmt.Sprintf("%s.%d", prefix, idx)

		var m map[string]string
		switch t := schema.Elem.(type) {
		case *Resource:
			m = d.stateObject(key, t.Schema)
		case *Schema:
			m = d.stateSingle(key, t)
		}

		for k, v := range m {
			result[k] = v
		}
	}

	return result
}

func (d *ResourceData) stateSingle(
	prefix string,
	schema *Schema) map[string]string {
	switch schema.Type {
	case TypeList:
		return d.stateList(prefix, schema)
	case TypeMap:
		return d.stateMap(prefix, schema)
	case TypeSet:
		return d.stateSet(prefix, schema)
	case TypeBool:
		fallthrough
	case TypeInt:
		fallthrough
	case TypeString:
		return d.statePrimitive(prefix, schema)
	default:
		panic(fmt.Sprintf("%s: unknown type %#v", prefix, schema.Type))
	}
}

func (d *ResourceData) stateSource(prefix string) getSource {
	// If we're not doing a partial apply, then get the set level
	if !d.partial {
		return getSourceSet | getSourceDiff
	}

	// Otherwise, only return getSourceSet if its in the partial map.
	// Otherwise we use state level only.
	for k, _ := range d.partialMap {
		if strings.HasPrefix(prefix, k) {
			return getSourceSet | getSourceDiff
		}
	}

	return getSourceState
}
