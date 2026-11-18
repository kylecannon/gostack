package internalapi

// Plugin is one entry of /_localstack/plugins.
type Plugin struct {
	// Name is the plugin's name.
	Name string
	// Initialized is whether the plugin has been initialized.
	Initialized bool
	// Loaded is whether the plugin has been loaded.
	Loaded bool
}
