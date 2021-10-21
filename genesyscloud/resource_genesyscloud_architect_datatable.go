package genesyscloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/mypurecloud/platform-client-sdk-go/v56/platformclientv2"
)

var (
	datatableProperty = &schema.Resource{
		Schema: map[string]*schema.Schema{
			"name": {
				Description: "Name of the property.",
				Type:        schema.TypeString,
				Required:    true,
			},
			"type": {
				Description:  "Type of the property (boolean | string | integer | number).",
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringInSlice([]string{"boolean", "string", "integer", "number"}, false),
			},
			"title": {
				Description: "Display title of the property.",
				Type:        schema.TypeString,
				Optional:    true,
			},
			"default": {
				Description: "Default value of the property. This is converted to the proper type for non-strings (e.g. set 'true' or 'false' for booleans).",
				Type:        schema.TypeString,
				Optional:    true,
			},
		},
	}
)

func getAllArchitectDatatables(ctx context.Context, clientConfig *platformclientv2.Configuration) (ResourceIDMetaMap, diag.Diagnostics) {
	resources := make(ResourceIDMetaMap)
	archAPI := platformclientv2.NewArchitectApiWithConfig(clientConfig)

	for pageNum := 1; ; pageNum++ {
		tables, _, getErr := archAPI.GetFlowsDatatables("", pageNum, 100, "", "", nil, "")
		if getErr != nil {
			return nil, diag.Errorf("Failed to get page of datatables: %v", getErr)
		}

		if tables.Entities == nil || len(*tables.Entities) == 0 {
			break
		}

		for _, table := range *tables.Entities {
			resources[*table.Id] = &ResourceMeta{Name: *table.Name}
		}
	}

	return resources, nil
}

func architectDatatableExporter() *ResourceExporter {
	return &ResourceExporter{
		GetResourcesFunc: getAllWithPooledClient(getAllArchitectDatatables),
		RefAttrs: map[string]*RefAttrSettings{
			"division_id": {RefType: "genesyscloud_auth_division"},
		},
	}
}

func resourceArchitectDatatable() *schema.Resource {
	return &schema.Resource{
		Description: "Genesys Cloud Architect Datatables",

		CreateContext: createWithPooledClient(createArchitectDatatable),
		ReadContext:   readWithPooledClient(readArchitectDatatable),
		UpdateContext: updateWithPooledClient(updateArchitectDatatable),
		DeleteContext: deleteWithPooledClient(deleteArchitectDatatable),
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		SchemaVersion: 1,
		Schema: map[string]*schema.Schema{
			"name": {
				Description: "Name of the datatable.",
				Type:        schema.TypeString,
				Required:    true,
			},
			"division_id": {
				Description: "The division to which this datatable will belong. If not set, the home division will be used.",
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
			},
			"description": {
				Description: "Description of the datatable.",
				Type:        schema.TypeString,
				Optional:    true,
			},
			"properties": {
				Description: "Schema properties of the datatable. This must at a minimum contain a string property 'key' that will serve as the row key. Properties cannot be removed from a schema once they have been added",
				Type:        schema.TypeList,
				Required:    true,
				MinItems:    1,
				Elem:        datatableProperty,
			},
		},
	}
}

func createArchitectDatatable(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	name := d.Get("name").(string)
	divisionID := d.Get("division_id").(string)
	description := d.Get("description").(string)

	sdkConfig := meta.(*providerMeta).ClientConfig
	archAPI := platformclientv2.NewArchitectApiWithConfig(sdkConfig)

	log.Printf("Creating datatable %s", name)

	schema, diagErr := buildSdkDatatableSchema(d)
	if diagErr != nil {
		return diagErr
	}

	datatable := &Datatable{
		Name:   &name,
		Schema: schema,
	}
	// Optional
	if divisionID != "" {
		datatable.Division = &platformclientv2.Writabledivision{Id: &divisionID}
	}

	if description != "" {
		datatable.Description = &description
	}

	table, _, err := sdkPutOrPostArchitectDatatable("POST", datatable, archAPI)
	if err != nil {
		return diag.Errorf("Failed to create datatable %s: %s", name, err)
	}

	d.SetId(*table.Id)

	log.Printf("Created datatable %s %s", name, *table.Id)
	return readArchitectDatatable(ctx, d, meta)
}

func readArchitectDatatable(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	sdkConfig := meta.(*providerMeta).ClientConfig
	archAPI := platformclientv2.NewArchitectApiWithConfig(sdkConfig)

	log.Printf("Reading datatable %s", d.Id())

	return withRetriesForRead(ctx, 30*time.Second, d, func() *resource.RetryError {
		datatable, resp, getErr := sdkGetArchitectDatatable(d.Id(), "schema", archAPI)
		if getErr != nil {
			if isStatus404(resp) {
				return resource.RetryableError(fmt.Errorf("Failed to read datatable %s: %s", d.Id(), getErr))
			}
			return resource.NonRetryableError(fmt.Errorf("Failed to read datatable %s: %s", d.Id(), getErr))
		}
		d.Set("name", *datatable.Name)
		d.Set("division_id", *datatable.Division.Id)

		if datatable.Description != nil {
			d.Set("description", *datatable.Description)
		} else {
			d.Set("description", nil)
		}

		if datatable.Schema != nil && datatable.Schema.Properties != nil {
			d.Set("properties", flattenDatatableProperties(*datatable.Schema.Properties))
		} else {
			d.Set("properties", nil)
		}

		log.Printf("Read datatable %s %s", d.Id(), *datatable.Name)

		return nil
	})
}

func updateArchitectDatatable(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	id := d.Id()
	name := d.Get("name").(string)
	divisionID := d.Get("division_id").(string)
	description := d.Get("description").(string)

	sdkConfig := meta.(*providerMeta).ClientConfig
	archAPI := platformclientv2.NewArchitectApiWithConfig(sdkConfig)

	log.Printf("Updating datatable %s", name)

	schema, diagErr := buildSdkDatatableSchema(d)
	if diagErr != nil {
		return diagErr
	}

	datatable := &Datatable{
		Id:     &id,
		Name:   &name,
		Schema: schema,
	}
	// Optional
	if divisionID != "" {
		datatable.Division = &platformclientv2.Writabledivision{Id: &divisionID}
	}

	if description != "" {
		datatable.Description = &description
	}

	_, _, err := sdkPutOrPostArchitectDatatable("PUT", datatable, archAPI)
	if err != nil {
		return diag.Errorf("Failed to update datatable %s: %s", name, err)
	}

	log.Printf("Updated datatable %s", name)
	time.Sleep(5 * time.Second)
	return readArchitectDatatable(ctx, d, meta)
}

func deleteArchitectDatatable(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	name := d.Get("name").(string)

	sdkConfig := meta.(*providerMeta).ClientConfig
	archAPI := platformclientv2.NewArchitectApiWithConfig(sdkConfig)

	log.Printf("Deleting datatable %s", name)
	_, err := archAPI.DeleteFlowsDatatable(d.Id(), true)
	if err != nil {
		return diag.Errorf("Failed to delete datatable %s: %s", name, err)
	}

	return withRetries(ctx, 30*time.Second, func() *resource.RetryError {
		_, resp, err := archAPI.GetFlowsDatatable(d.Id(), "")
		if err != nil {
			if isStatus404(resp) {
				// Datatable row deleted
				log.Printf("Deleted datatable row %s", name)
				return nil
			}
			return resource.NonRetryableError(fmt.Errorf("Error deleting datatable row %s: %s", name, err))
		}
		return resource.RetryableError(fmt.Errorf("Datatable row %s still exists", name))
	})
}

func buildSdkDatatableSchema(d *schema.ResourceData) (*Jsonschemadocument, diag.Diagnostics) {
	// Hardcoded values the server expects in the JSON schema object
	var (
		schemaType           = "http://json-schema.org/draft-04/schema#"
		jsonType             = "object"
		additionalProperties interface{}
	)

	additionalProperties = false
	properties, err := buildSdkDatatableProperties(d)
	if err != nil {
		return nil, err
	}
	return &Jsonschemadocument{
		Schema:               &schemaType,
		VarType:              &jsonType,
		Required:             &[]string{"key"},
		Properties:           properties,
		AdditionalProperties: &additionalProperties,
	}, nil
}

func buildSdkDatatableProperties(d *schema.ResourceData) (*map[string]Datatableproperty, diag.Diagnostics) {
	const propIdPrefix = "/properties/"
	if properties := d.Get("properties").([]interface{}); properties != nil {
		sdkProps := map[string]Datatableproperty{}
		for i, property := range properties {
			propMap := property.(map[string]interface{})

			// Name and type are required
			propName := propMap["name"].(string)
			propType := propMap["type"].(string)
			propId := propIdPrefix + propName
			orderNum := i

			sdkProp := Datatableproperty{
				Id:           &propId,
				DisplayOrder: &orderNum,
				VarType:      &propType,
			}

			// Title is optional
			if propTitle, ok := propMap["title"]; ok {
				title := propTitle.(string)
				sdkProp.Title = &title
			}

			// Default is optional
			if propDefault, ok := propMap["default"]; ok {
				def := propDefault.(string)
				var defaultVal interface{}
				if def != "" {
					var err error
					// Convert default value to the appropriate type
					switch propType {
					case "boolean":
						defaultVal, err = strconv.ParseBool(def)
					case "string":
						defaultVal = def
					case "integer":
						defaultVal, err = strconv.Atoi(def)
					case "number":
						defaultVal, err = strconv.ParseFloat(def, 64)
					default:
						return nil, diag.Errorf("Invalid type %s for Datatable property %s", propType, propName)
					}
					if err != nil {
						return nil, diag.FromErr(err)
					}
				}
				if defaultVal != nil {
					sdkProp.Default = &defaultVal
				}
			}
			sdkProps[propName] = sdkProp
		}
		return &sdkProps, nil
	}
	return nil, nil
}

func flattenDatatableProperties(properties map[string]Datatableproperty) []interface{} {
	configProps := []interface{}{}

	type kv struct {
		Key   string
		Value Datatableproperty
	}

	var propList []kv
	defaultOrder := 0
	for k, v := range properties {
		if v.DisplayOrder == nil {
			// Set a default so the sort doesn't fail
			v.DisplayOrder = &defaultOrder
		}
		propList = append(propList, kv{k, v})
	}

	// Sort by display order
	sort.SliceStable(propList, func(i, j int) bool {
		return *propList[i].Value.DisplayOrder < *propList[j].Value.DisplayOrder
	})

	for _, propKV := range propList {
		propMap := make(map[string]interface{})

		propMap["name"] = propKV.Key
		if propKV.Value.VarType != nil {
			propMap["type"] = *propKV.Value.VarType
		}
		if propKV.Value.Title != nil {
			propMap["title"] = *propKV.Value.Title
		}
		if propKV.Value.Default != nil {
			propMap["default"] = interfaceToString(*propKV.Value.Default)
		}
		configProps = append(configProps, propMap)
	}
	return configProps
}

type Datatableproperty struct {
	Id           *string      `json:"$id,omitempty"`
	VarType      *string      `json:"type,omitempty"`
	Title        *string      `json:"title,omitempty"`
	Default      *interface{} `json:"default,omitempty"`
	DisplayOrder *int         `json:"displayOrder,omitempty"`
}

// Overriding the SDK Datatable document as it does not allow setting additionalProperties to 'false' as required by the API
type Jsonschemadocument struct {
	Schema               *string                       `json:"$schema,omitempty"`
	VarType              *string                       `json:"type,omitempty"`
	Required             *[]string                     `json:"required,omitempty"`
	Properties           *map[string]Datatableproperty `json:"properties,omitempty"`
	AdditionalProperties *interface{}                  `json:"additionalProperties,omitempty"`
}

type Datatable struct {
	Id          *string                            `json:"id,omitempty"`
	Name        *string                            `json:"name,omitempty"`
	Description *string                            `json:"description,omitempty"`
	Division    *platformclientv2.Writabledivision `json:"division,omitempty"`
	Schema      *Jsonschemadocument                `json:"schema,omitempty"`
}

func sdkPutOrPostArchitectDatatable(method string, body *Datatable, api *platformclientv2.ArchitectApi) (*Datatable, *platformclientv2.APIResponse, error) {
	apiClient := &api.Configuration.APIClient

	// create path and map variables
	path := api.Configuration.BasePath + "/api/v2/flows/datatables"
	if method == "PUT" && body.Id != nil {
		path += "/" + *body.Id
	}

	headerParams := make(map[string]string)

	// add default headers if any
	for key := range api.Configuration.DefaultHeader {
		headerParams[key] = api.Configuration.DefaultHeader[key]
	}

	headerParams["Authorization"] = "Bearer " + api.Configuration.AccessToken
	headerParams["Content-Type"] = "application/json"
	headerParams["Accept"] = "application/json"

	var successPayload *Datatable
	response, err := apiClient.CallAPI(path, method, body, headerParams, nil, nil, "", nil)
	if err != nil {
		// Nothing special to do here, but do avoid processing the response
	} else if err == nil && response.Error != nil {
		err = errors.New(response.ErrorMessage)
	} else {
		err = json.Unmarshal([]byte(response.RawBody), &successPayload)
	}
	return successPayload, response, err
}

func sdkGetArchitectDatatable(datatableId string, expand string, api *platformclientv2.ArchitectApi) (*Datatable, *platformclientv2.APIResponse, error) {
	apiClient := &api.Configuration.APIClient

	// create path and map variables
	path := api.Configuration.BasePath + "/api/v2/flows/datatables/" + datatableId

	headerParams := make(map[string]string)
	queryParams := make(map[string]string)

	// oauth required
	if api.Configuration.AccessToken != "" {
		headerParams["Authorization"] = "Bearer " + api.Configuration.AccessToken
	}
	// add default headers if any
	for key := range api.Configuration.DefaultHeader {
		headerParams[key] = api.Configuration.DefaultHeader[key]
	}

	queryParams["expand"] = apiClient.ParameterToString(expand, "")

	headerParams["Content-Type"] = "application/json"
	headerParams["Accept"] = "application/json"

	var successPayload *Datatable
	response, err := apiClient.CallAPI(path, "GET", nil, headerParams, queryParams, nil, "", nil)
	if err != nil {
		// Nothing special to do here, but do avoid processing the response
	} else if err == nil && response.Error != nil {
		err = errors.New(response.ErrorMessage)
	} else {
		err = json.Unmarshal([]byte(response.RawBody), &successPayload)
	}
	return successPayload, response, err
}
