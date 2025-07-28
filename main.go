// main.go
// Environment Variable Manager - A Windows GUI application for managing user and system environment variables
// Supports importing/exporting YAML configurations and requires administrator privileges for system variables
package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	sqweekdialog "github.com/sqweek/dialog"
	"golang.org/x/sys/windows/registry"
	"gopkg.in/yaml.v2"
)

// Variable represents a single environment variable with its operation type
type Variable struct {
	Name      string `yaml:"name"`      // Environment variable name
	Value     string `yaml:"value"`     // Environment variable value
	Operation string `yaml:"operation"` // "set" to create/update, "delete" to remove
}

// Config represents the structure of a YAML configuration file
type Config struct {
	UserVariables   []Variable `yaml:"user_variables"`   // Variables for current user only
	SystemVariables []Variable `yaml:"system_variables"` // System-wide variables (requires admin)
}

const (
	HWND_BROADCAST   = 0xffff // Send message to all top-level windows
	WM_SETTINGCHANGE = 0x001A // Windows message for environment variable changes
)

func main() {
	// Initialize Fyne application with dark theme
	myApp := app.New()
	myApp.Settings().SetTheme(theme.DarkTheme())
	myWindow := myApp.NewWindow("Environment Variable Manager")
	myWindow.Resize(fyne.NewSize(600, 400))

	// Check if running with administrator privileges
	isAdmin, err := isRunningAsAdmin()
	if err != nil {
		log.Printf("Warning: Could not determine admin status: %v", err)
	}

	adminStatus := "Standard User"
	if isAdmin {
		adminStatus = "Administrator"
	}

	// Initialize UI state variables
	selectedFilePath := ""
	// Check if a config file was passed as command line argument (used during UAC elevation)
	if len(os.Args) > 1 {
		selectedFilePath = os.Args[1]
	}

	// Create UI labels for file path and status feedback
	filePathLabel := widget.NewLabel("No file selected.")
	if selectedFilePath != "" {
		filePathLabel.SetText(fmt.Sprintf("Selected: %s", selectedFilePath))
	}

	statusLabel := widget.NewLabel("Ready. Please select a YAML config file.")
	if selectedFilePath != "" {
		statusLabel.SetText("File pre-selected. Click 'Preview Changes' or 'Apply Variables' to proceed.")
	}

	// Handler function to preview changes without applying them
	previewChanges := func() {
		if selectedFilePath == "" {
			dialog.ShowInformation("Error", "Please select a YAML configuration file first.", myWindow)
			return
		}

		// Validate file extension before processing
		if !isValidYAMLFile(selectedFilePath) {
			dialog.ShowError(fmt.Errorf("invalid file type: please select a valid YAML file (.yaml or .yml extension)"), myWindow)
			return
		}

		yamlFile, err := ioutil.ReadFile(selectedFilePath)
		if err != nil {
			dialog.ShowError(fmt.Errorf("error reading YAML file %s: %v", selectedFilePath, err), myWindow)
			return
		}

		var config Config
		err = yaml.Unmarshal(yamlFile, &config)
		if err != nil {
			dialog.ShowError(fmt.Errorf("error unmarshaling YAML: %v", err), myWindow)
			return
		}

		showPreviewWindow(myApp, config, isAdmin)
	}

	// Handler function to apply environment variables from selected YAML file
	applyEnvVars := func() {
		if selectedFilePath == "" {
			dialog.ShowInformation("Error", "Please select a YAML configuration file first.", myWindow)
			return
		}

		// Validate file extension before processing
		if !isValidYAMLFile(selectedFilePath) {
			dialog.ShowError(fmt.Errorf("invalid file type: please select a valid YAML file (.yaml or .yml extension)"), myWindow)
			return
		}

		statusLabel.SetText("Applying variables... Please wait.")
		statusLabel.Refresh()

		// Run in goroutine to prevent UI blocking during registry operations
		go func() {
			yamlFile, err := ioutil.ReadFile(selectedFilePath)
			if err != nil {
				statusLabel.SetText(fmt.Sprintf("Error reading YAML file: %v", err))
				dialog.ShowError(fmt.Errorf("error reading YAML file %s: %v", selectedFilePath, err), myWindow)
				statusLabel.Refresh()
				return
			}

			var config Config
			err = yaml.Unmarshal(yamlFile, &config)
			if err != nil {
				statusLabel.SetText(fmt.Sprintf("Error unmarshaling YAML: %v", err))
				dialog.ShowError(fmt.Errorf("error unmarshaling YAML: %v", err), myWindow)
				statusLabel.Refresh()
				return
			}

			// Apply user environment variables (always accessible)
			fmt.Println("Applying user environment variables...")
			if err := applyVariables(config.UserVariables, registry.CURRENT_USER, "Environment"); err != nil {
				statusLabel.SetText(fmt.Sprintf("Error applying user variables: %v", err))
				dialog.ShowError(fmt.Errorf("error applying user variables: %v", err), myWindow)
				statusLabel.Refresh()
				return
			}

			// Apply system environment variables (requires administrator privileges)
			if isAdmin {
				fmt.Println("Applying system environment variables...")
				if err := applyVariables(config.SystemVariables, registry.LOCAL_MACHINE, "SYSTEM\\CurrentControlSet\\Control\\Session Manager\\Environment"); err != nil {
					statusLabel.SetText(fmt.Sprintf("Error applying system variables: %v", err))
					dialog.ShowError(fmt.Errorf("error applying system variables: %v", err), myWindow)
					statusLabel.Refresh()
					return
				}
			} else if len(config.SystemVariables) > 0 {
				// Inform user that system variables were skipped due to insufficient privileges
				statusLabel.SetText("System variables were ignored. Relaunch as admin to apply them.")
				dialog.ShowInformation("Admin Required", "To apply system environment variables, please relaunch the app as Administrator.", myWindow)
				statusLabel.Refresh()
				return
			}

			// Broadcast WM_SETTINGCHANGE to notify other applications of environment changes
			fmt.Println("Broadcasting WM_SETTINGCHANGE message...")
			if err := broadcastSettingChange(); err != nil {
				statusLabel.SetText(fmt.Sprintf("Error broadcasting changes: %v", err))
				dialog.ShowError(fmt.Errorf("error broadcasting WM_SETTINGCHANGE: %v", err), myWindow)
				statusLabel.Refresh()
			} else {
				statusLabel.SetText("Environment variables applied successfully. Some applications may need to be restarted.")
				dialog.ShowInformation("Success", "Environment variables applied successfully.\n\nPlease note: Some applications (like Explorer, Command Prompt, PowerShell) may need to be restarted to reflect the changes.", myWindow)
				statusLabel.Refresh()
			}
		}()
	}

	// Create UI buttons with their respective handlers
	chooseFileButton := widget.NewButton("Choose YAML Config File", func() {
		// Run file dialog in goroutine to prevent UI blocking
		go func() {
			filePath, err := sqweekdialog.File().Filter("YAML Config", "yaml", "yml").Load()
			if err != nil {
				if err.Error() == "cancelled" {
					statusLabel.SetText("File selection cancelled.")
				} else {
					statusLabel.SetText(fmt.Sprintf("Error choosing file: %v", err))
					dialog.ShowError(fmt.Errorf("error choosing file: %v", err), myWindow)
				}
				statusLabel.Refresh()
				return
			}
			selectedFilePath = filePath
			filePathLabel.SetText(fmt.Sprintf("Selected: %s", selectedFilePath))
			filePathLabel.Refresh()
			statusLabel.SetText("File selected. Click 'Preview Changes' or 'Apply Variables' to proceed.")
			statusLabel.Refresh()
		}()
	})

	previewButton := widget.NewButton("Preview Changes", previewChanges)
	applyButton := widget.NewButton("Apply Variables", applyEnvVars)

	// Button to relaunch application with administrator privileges
	runAsAdminButton := widget.NewButton("Relaunch as Admin", func() {
		go func() {
			// Preserve command line arguments when elevating
			args := os.Args[1:]
			if selectedFilePath != "" && !contains(args, selectedFilePath) {
				args = append(args, selectedFilePath)
			}

			err := elevateAsAdmin(args...)
			if err != nil {
				dialog.ShowError(fmt.Errorf("failed to relaunch as admin: %v", err), myWindow)
			} else {
				myApp.Quit()
			}
		}()
	})

	// Button to export current environment variables to YAML file
	exportButton := widget.NewButton("Export Variables to YAML", func() {
		go func() {
			statusLabel.SetText("Exporting variables... Please wait.")
			statusLabel.Refresh()

			configToExport, exportErr := exportEnvironmentVariables(isAdmin)
			if exportErr != nil {
				statusLabel.SetText(fmt.Sprintf("Error exporting variables: %v", exportErr))
				dialog.ShowError(fmt.Errorf("error exporting variables: %v", exportErr), myWindow)
				statusLabel.Refresh()
				return
			}

			savePath, err := sqweekdialog.File().Filter("YAML Config", "yaml", "yml").Save()
			if err != nil {
				if err.Error() == "cancelled" {
					statusLabel.SetText("Export cancelled.")
				} else {
					statusLabel.SetText(fmt.Sprintf("Error saving file: %v", err))
					dialog.ShowError(fmt.Errorf("error saving file: %v", err), myWindow)
				}
				statusLabel.Refresh()
				return
			}

			if savePath == "" {
				statusLabel.SetText("Export cancelled.")
				statusLabel.Refresh()
				return
			}

			// Ensure exported file has proper YAML extension
			if !strings.HasSuffix(strings.ToLower(savePath), ".yaml") && !strings.HasSuffix(strings.ToLower(savePath), ".yml") {
				savePath += ".yaml"
			}

			if saveErr := saveConfigToFile(configToExport, savePath); saveErr != nil {
				statusLabel.SetText(fmt.Sprintf("Error writing config to file: %v", saveErr))
				dialog.ShowError(fmt.Errorf("error writing config to file: %v", saveErr), myWindow)
				statusLabel.Refresh()
			} else {
				statusLabel.SetText(fmt.Sprintf("Variables exported successfully to: %s", savePath))
				dialog.ShowInformation("Export Success", fmt.Sprintf("All current environment variables exported to:\n%s", savePath), myWindow)
				statusLabel.Refresh()
			}
		}()
	})

	// Layout all UI components vertically
	content := container.NewVBox(
		widget.NewLabel("This application manages Windows user and system environment variables."),
		widget.NewLabel("Click 'Choose YAML Config File' to select your configuration."),
		chooseFileButton,
		filePathLabel,
		previewButton,
		applyButton,
		exportButton,
		runAsAdminButton,
		widget.NewLabel(fmt.Sprintf("Privilege Level: %s", adminStatus)),
		widget.NewSeparator(),
		statusLabel,
	)

	myWindow.SetContent(content)
	myWindow.ShowAndRun()
}

// isValidYAMLFile checks if the provided file path has a valid YAML extension
func isValidYAMLFile(filePath string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))
	return ext == ".yaml" || ext == ".yml"
}

// showPreviewWindow creates and displays a window showing all pending environment variable changes
func showPreviewWindow(app fyne.App, config Config, isAdmin bool) {
	previewWindow := app.NewWindow("Preview Changes")
	previewWindow.Resize(fyne.NewSize(700, 500))

	var content []string

	// Display user environment variables section
	if len(config.UserVariables) > 0 {
		content = append(content, "USER ENVIRONMENT VARIABLES:")
		content = append(content, "")
		for _, v := range config.UserVariables {
			switch v.Operation {
			case "set":
				content = append(content, fmt.Sprintf("  SET: %s = %s", v.Name, v.Value))
			case "delete":
				content = append(content, fmt.Sprintf("  DELETE: %s", v.Name))
			default:
				content = append(content, fmt.Sprintf("  UNKNOWN OPERATION (%s): %s = %s", v.Operation, v.Name, v.Value))
			}
		}
		content = append(content, "")
	}

	// Display system environment variables section with admin warning
	if len(config.SystemVariables) > 0 {
		content = append(content, "SYSTEM ENVIRONMENT VARIABLES:")
		if !isAdmin {
			content = append(content, "  ⚠️  WARNING: Running as standard user - system variables will be IGNORED")
		}
		content = append(content, "")
		for _, v := range config.SystemVariables {
			prefix := "  "
			if !isAdmin {
				prefix = "  [IGNORED] "
			}
			switch v.Operation {
			case "set":
				content = append(content, fmt.Sprintf("%sSET: %s = %s", prefix, v.Name, v.Value))
			case "delete":
				content = append(content, fmt.Sprintf("%sDELETE: %s", prefix, v.Name))
			default:
				content = append(content, fmt.Sprintf("%sUNKNOWN OPERATION (%s): %s = %s", prefix, v.Operation, v.Name, v.Value))
			}
		}
		content = append(content, "")
	}

	if len(config.UserVariables) == 0 && len(config.SystemVariables) == 0 {
		content = append(content, "No environment variables found in the configuration file.")
	}

	content = append(content, "")
	content = append(content, "Note: After applying changes, a WM_SETTINGCHANGE message will be")
	content = append(content, "broadcast to notify other applications of the environment changes.")

	// Use a Label for better theme compatibility and automatic text color handling
	previewLabel := widget.NewLabel(strings.Join(content, "\n"))
	previewLabel.Wrapping = fyne.TextWrapWord
	previewLabel.Alignment = fyne.TextAlignLeading

	scrollContainer := container.NewScroll(previewLabel)
	scrollContainer.SetMinSize(fyne.NewSize(680, 400))

	closeButton := widget.NewButton("Close", func() {
		previewWindow.Close()
	})

	windowContent := container.NewVBox(
		widget.NewLabel("The following changes will be made to your environment variables:"),
		widget.NewSeparator(),
		scrollContainer,
		widget.NewSeparator(),
		container.NewHBox(closeButton),
	)

	previewWindow.SetContent(windowContent)
	previewWindow.Show()
}

// applyVariables processes a list of environment variables and applies them to the Windows registry
func applyVariables(variables []Variable, hive registry.Key, subkeyPath string) error {
	// Get human-readable hive name for error messages
	var hiveName string
	switch hive {
	case registry.CURRENT_USER:
		hiveName = "HKEY_CURRENT_USER"
	case registry.LOCAL_MACHINE:
		hiveName = "HKEY_LOCAL_MACHINE"
	default:
		hiveName = fmt.Sprintf("UnknownHive(%d)", hive)
	}

	// Open registry key with write permissions
	key, err := registry.OpenKey(hive, subkeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("failed to open registry key %s\\%s: %w", hiveName, subkeyPath, err)
	}
	defer key.Close()

	// Process each variable according to its operation type
	for _, v := range variables {
		switch v.Operation {
		case "set":
			if err := key.SetStringValue(v.Name, v.Value); err != nil {
				fmt.Printf("  Failed to set %s=%s: %v\n", v.Name, v.Value, err)
			} else {
				fmt.Printf("  Successfully set %s=%s\n", v.Name, v.Value)
			}
		case "delete":
			if err := key.DeleteValue(v.Name); err != nil {
				if os.IsNotExist(err) {
					fmt.Printf("  Variable %s already deleted or did not exist.\n", v.Name)
				} else {
					fmt.Printf("  Failed to delete %s: %v\n", v.Name, err)
				}
			} else {
				fmt.Printf("  Successfully deleted %s\n", v.Name)
			}
		default:
			fmt.Printf("  Unknown operation '%s' for variable %s. Skipping.\n", v.Operation, v.Name)
		}
	}
	return nil
}

// broadcastSettingChange notifies all Windows applications that environment variables have changed
// This allows applications like Explorer and Command Prompt to pick up the new values
func broadcastSettingChange() error {
	user32 := syscall.NewLazyDLL("user32.dll")
	sendMessageTimeout := user32.NewProc("SendMessageTimeoutW")

	environmentStrPtr := uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("Environment")))

	// Call Windows API to broadcast the environment change message
	ret, _, err := sendMessageTimeout.Call(
		uintptr(HWND_BROADCAST),   // Send to all top-level windows
		uintptr(WM_SETTINGCHANGE), // Environment setting changed message
		0,                         // wParam (unused)
		environmentStrPtr,         // lParam (pointer to "Environment" string)
		0,                         // Normal message sending
		5000,                      // 5 second timeout
		0,                         // Return value (unused)
	)

	if ret == 0 {
		return fmt.Errorf("SendMessageTimeoutW failed: %w", err)
	}
	return nil
}

// exportEnvironmentVariables reads all current environment variables from the Windows registry
func exportEnvironmentVariables(isAdmin bool) (Config, error) {
	var config Config
	var err error

	// Always export user variables (accessible to all users)
	config.UserVariables, err = readVariablesFromRegistry(registry.CURRENT_USER, "Environment")
	if err != nil {
		return Config{}, fmt.Errorf("failed to read user environment variables: %w", err)
	}

	// Only export system variables if running as administrator
	if isAdmin {
		config.SystemVariables, err = readVariablesFromRegistry(registry.LOCAL_MACHINE, "SYSTEM\\CurrentControlSet\\Control\\Session Manager\\Environment")
		if err != nil {
			return Config{}, fmt.Errorf("failed to read system environment variables: %w", err)
		}
	} else {
		fmt.Println("Skipping system environment variable export: Application not running as Administrator.")
	}

	return config, nil
}

// readVariablesFromRegistry reads all environment variables from a specific registry location
func readVariablesFromRegistry(hive registry.Key, subkeyPath string) ([]Variable, error) {
	var variables []Variable
	// Get human-readable hive name for error messages
	var hiveName string
	switch hive {
	case registry.CURRENT_USER:
		hiveName = "HKEY_CURRENT_USER"
	case registry.LOCAL_MACHINE:
		hiveName = "HKEY_LOCAL_MACHINE"
	default:
		hiveName = fmt.Sprintf("UnknownHive(%d)", hive)
	}

	// Open registry key with read permissions
	key, err := registry.OpenKey(hive, subkeyPath, registry.READ)
	if err != nil {
		return nil, fmt.Errorf("failed to open registry key %s\\%s for reading: %w", hiveName, subkeyPath, err)
	}
	defer key.Close()

	// Get all value names in the registry key
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return nil, fmt.Errorf("failed to read value names from registry key: %w", err)
	}

	// Read each environment variable value
	for _, name := range names {
		value, _, err := key.GetStringValue(name)
		if err != nil {
			fmt.Printf("  Warning: Could not read value for %s: %v\n", name, err)
			continue
		}
		variables = append(variables, Variable{Name: name, Value: value, Operation: "set"})
	}
	return variables, nil
}

// saveConfigToFile marshals a Config struct to YAML format and saves it to disk
func saveConfigToFile(config Config, filePath string) error {
	yamlData, err := yaml.Marshal(&config)
	if err != nil {
		return fmt.Errorf("failed to marshal config to YAML: %w", err)
	}

	// Write YAML data to file with standard permissions
	err = ioutil.WriteFile(filePath, yamlData, 0644)
	if err != nil {
		return fmt.Errorf("failed to write YAML to file %s: %w", filePath, err)
	}
	return nil
}

// isRunningAsAdmin checks if the current process has administrator privileges using Windows API
func isRunningAsAdmin() (bool, error) {
	shell32 := syscall.NewLazyDLL("shell32.dll")
	isUserAnAdmin := shell32.NewProc("IsUserAnAdmin")

	// Call Windows API function
	ret, _, callErr := isUserAnAdmin.Call()
	if callErr != syscall.Errno(0) {
		return false, callErr
	}
	return ret != 0, nil
}

// elevateAsAdmin relaunches the current executable with administrator privileges via UAC
// All provided arguments are passed to the elevated process
func elevateAsAdmin(args ...string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find executable path: %w", err)
	}
	verb := "runas" // UAC elevation verb
	cwd, _ := os.Getwd()

	// Join all arguments into a single string for ShellExecuteW
	argv := strings.Join(args, " ")

	// Convert strings to UTF-16 pointers as required by Windows API
	verbPtr, _ := syscall.UTF16PtrFromString(verb)
	exePtr, _ := syscall.UTF16PtrFromString(exePath)
	paramPtr, _ := syscall.UTF16PtrFromString(argv)
	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)

	// Call ShellExecuteW to launch elevated process
	r, _, err := syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW").Call(
		0, // hWnd (no parent window)
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(exePtr)),
		uintptr(unsafe.Pointer(paramPtr)),
		uintptr(unsafe.Pointer(cwdPtr)),
		syscall.SW_NORMAL, // Show window normally
	)

	// ShellExecuteW returns > 32 on success
	if r <= 32 {
		return fmt.Errorf("ShellExecuteW failed: %w", err)
	}
	return nil
}

// contains is a helper function to check if a string exists in a slice of strings
func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}
