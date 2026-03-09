package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	humanize "github.com/dustin/go-humanize"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	vbox "github.com/terra-farm/go-virtualbox"
)

var (
	defaultBootOrder = []string{"disk", "none", "none", "none"}
)

func init() {
	vbox.Verbose = true
}

// isAPIPA checks if an IP address is an APIPA (Automatic Private IP Addressing) address.
// APIPA addresses are in the range 169.254.0.0/16 and are self-assigned when DHCP fails.
func isAPIPA(ipAddr string) bool {
	ip := net.ParseIP(ipAddr)
	if ip == nil {
		return false
	}
	// APIPA range: 169.254.0.0 to 169.254.255.255
	_, apipa, _ := net.ParseCIDR("169.254.0.0/16")
	return apipa.Contains(ip)
}

// isValidIPAddress checks if an IP address is valid and not an APIPA address.
func isValidIPAddress(ipAddr string) bool {
	if ipAddr == "" {
		return false
	}
	return !isAPIPA(ipAddr)
}

func resourceVM() *schema.Resource {
	return &schema.Resource{
		Exists:        resourceVMExists,
		CreateContext: resourceVMCreate,
		ReadContext:   resourceVMRead,
		UpdateContext: resourceVMUpdate,
		Delete:        resourceVMDelete,

		Schema: map[string]*schema.Schema{

			"name": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"image": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"url": {
				Type:       schema.TypeString,
				Optional:   true,
				ForceNew:   true,
				Deprecated: "Use the \"image\" option with a URL",
			},

			"optical_disks": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "List of Optical Disks to attach",
				Elem:        &schema.Schema{Type: schema.TypeString},
			},

			"cpus": {
				Type:     schema.TypeInt,
				Optional: true,
				Default:  "2",
			},

			"ostype": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "Linux_64",
				Description: "VirtualBox OS type ID (see VBoxManage list ostypes)",
			},

			"memory": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  "512mib",
			},

			"status": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  "running",
			},

			"user_data": {
				Deprecated: "user_data is not working and is temporarily deprecated while we figure out how to make it work",
				Type:       schema.TypeString,
				Optional:   true,
				Default:    "",
			},

			"checksum": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  "",
			},

			"checksum_type": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  "",
			},

			"disk_size": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "Disk size for the VM, allows human friendly units like '10GB', '20GiB', '500MiB'. If set, disks will be resized to this value. Expansion only — must be larger than current size. Shrinking is not supported. VMDK disks are converted to VDI for resizing.",
			},

			"network_adapter": {
				Type:     schema.TypeList,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{

						"type": {
							Type:     schema.TypeString,
							Required: true,
						},

						"device": {
							Type:     schema.TypeString,
							Optional: true,
							Default:  "IntelPro1000MTServer",
						},

						"host_interface": {
							Type:     schema.TypeString,
							Optional: true,
						},

						"status": {
							Type:     schema.TypeString,
							Computed: true,
						},

						"mac_address": {
							Type:     schema.TypeString,
							Computed: true,
						},

						"ipv4_address": {
							Type:     schema.TypeString,
							Computed: true,
						},

						"ipv4_address_available": {
							Type:     schema.TypeString,
							Computed: true,
						},
					},
				},
			},

			"boot_order": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "Boot order, max 4 slots, each in [none, floopy, dvd, disk, net]",
				Elem:        &schema.Schema{Type: schema.TypeString},
				MaxItems:    4,
			},
		},
	}
}

func resourceVMExists(d *schema.ResourceData, meta any) (bool, error) {
	name := d.Get("name").(string)

	switch _, err := vbox.GetMachine(name); err {
	case nil:
		return true, nil
	case vbox.ErrMachineNotExist:
		return false, nil
	default:
		return false, fmt.Errorf("error when checking for existance of the VM: %w", err)
	}
}

var imageOpMutex sync.Mutex

func resourceVMCreate(ctx context.Context, d *schema.ResourceData, meta any) diag.Diagnostics {
	image := d.Get("image").(string)

	if addr, exists := d.GetOk("url"); exists {
		image = addr.(string)
	}

	u, err := url.Parse(image)
	if err != nil {
		return diag.Errorf("could not parse image URL: %v", err)
	}

	imagePath, err := fetchIfRemote(u)
	if err != nil {
		return diag.Errorf("unable to fetch remote image: %v", err)
	}

	/* Get gold folder and machine folder */
	usr, err := user.Current()
	if err != nil {
		return diag.Errorf("unable to get the current user: %v", err)
	}
	goldFolder := filepath.Join(usr.HomeDir, ".terraform/virtualbox/gold")
	machineFolder := filepath.Join(usr.HomeDir, ".terraform/virtualbox/machine")
	err = os.MkdirAll(goldFolder, 0740)
	if err != nil {
		return diag.Errorf("unable to create gold folder: %v", err)
	}
	err = os.MkdirAll(machineFolder, 0740)
	if err != nil {
		return diag.Errorf("unable to create machine folder: %v", err)
	}

	// Unpack gold image to gold folder
	imageOpMutex.Lock() // Sequentialize image unpacking to avoid conflicts
	goldFileName := filepath.Base(imagePath)
	goldName := strings.TrimSuffix(goldFileName, filepath.Ext(goldFileName))
	if filepath.Ext(goldName) == ".tar" {
		goldName = strings.TrimSuffix(goldName, ".tar")
	}

	goldPath := filepath.Join(goldFolder, goldName)
	if err = unpackImage(ctx, imagePath, goldPath); err != nil {
		imageOpMutex.Unlock()
		return diag.Errorf("failed to unpack image %s: %v", image, err)
	}
	imageOpMutex.Unlock()

	// Gather '*.vdi' and "*.vmdk" files from gold
	goldDisks, err := gatherDisks(goldPath)
	if err != nil {
		return diag.Errorf("unable to gather disks: %v", err)
	}

	// Create VM instance
	name := d.Get("name").(string)
	vm, err := vbox.CreateMachine(name, machineFolder)
	if err != nil {
		return diag.Errorf("can't create virtualbox VM %s: %v", name, err)
	}

	// Clone gold virtual disk files to VM folder
	for _, src := range goldDisks {
		filename := filepath.Base(src)

		target := filepath.Join(vm.BaseFolder, filename)

		if _, _, err := vbox.Run(ctx, "internalcommands", "sethduuid", src); err != nil {
			return diag.Errorf("unable to set UUID: %v", err)
		}

		imageOpMutex.Lock() // Sequentialize image cloning to improve disk performance
		err := vbox.CloneHD(src, target)
		imageOpMutex.Unlock()
		if err != nil {
			return diag.Errorf("failed to clone *.vdi and *.vmdk to VM folder: %v", err)
		}
	}

	// Resize cloned disks if disk_size is specified
	if diskSizeStr, ok := d.GetOk("disk_size"); ok {
		clonedDisks, err := gatherDisks(vm.BaseFolder)
		if err != nil {
			return diag.Errorf("unable to gather disks for resizing: %v", err)
		}
		for _, disk := range clonedDisks {
			// Skip configdrive disks — these are small cloud-init metadata disks
			// that should not be resized
			if strings.Contains(strings.ToLower(filepath.Base(disk)), "configdrive") {
				tflog.Debug(ctx, "skipping configdrive disk from resizing", map[string]any{
					"disk": disk,
				})
				continue
			}
			resized, err := resizeDisk(ctx, disk, diskSizeStr.(string))
			if err != nil {
				return diag.Errorf("failed to resize disk %s: %v", disk, err)
			}
			// If the disk was converted (VMDK -> VDI), remove the old VMDK
			if resized != disk {
				if err := os.Remove(disk); err != nil {
					tflog.Warn(ctx, "failed to remove old VMDK after conversion", map[string]any{
						"disk":  disk,
						"error": err.Error(),
					})
				}
			}
		}
	}

	// Attach virtual disks to VM
	vmDisks, err := gatherDisks(vm.BaseFolder)
	if err != nil {
		return diag.Errorf("unable to gather disks: %v", err)
	}

	if err := vm.AddStorageCtl("SATA", vbox.StorageController{
		SysBus:      vbox.SysBusSATA,
		Ports:       uint(len(vmDisks)) + 1,
		Chipset:     vbox.CtrlIntelAHCI,
		HostIOCache: true,
		Bootable:    true,
	}); err != nil {
		return diag.Errorf("can't create VirtualBox storage controller: %v", err)
	}

	for i, disk := range vmDisks {
		if err := vm.AttachStorage("SATA", vbox.StorageMedium{
			Port:      uint(i),
			Device:    0,
			DriveType: vbox.DriveHDD,
			Medium:    disk,
		}); err != nil {
			return diag.Errorf("failed to attach VirtualBox storage medium: %v", err)
		}
	}

	opticalDiskCount := d.Get("optical_disks.#").(int)
	opticalDisks := make([]string, 0, opticalDiskCount)

	for i := 0; i < opticalDiskCount; i++ {
		attr := fmt.Sprintf("optical_disks.%d", i)
		if opticalDiskImage, ok := d.Get(attr).(string); ok && attr != "" {
			opticalDisks = append(opticalDisks, opticalDiskImage)
		}
	}

	for i := 0; i < len(opticalDisks); i++ {
		opticalDiskImage := opticalDisks[i]
		fileName := filepath.Base(opticalDiskImage)

		target := filepath.Join(vm.BaseFolder, fileName)

		sourceFile, err := os.Open(opticalDiskImage)
		if err != nil {
			return diag.Errorf("failed to open source optical disk image: %v", err)
		}

		// make sure the file is closed when this function ends
		defer sourceFile.Close()

		targetFile, err := os.Create(target)
		if err != nil {
			return diag.Errorf("failed to create target optical disk image: %v", err)
		}

		// make sure the file is closed when this function ends
		defer targetFile.Close()

		if _, err := io.Copy(targetFile, sourceFile); err != nil {
			return diag.Errorf("copy optical disk image failed: %v", err)
		}

		// Explicitly sync & close the file now, so virtualbox can read it immediately, if we do not
		// do this, attaching the iso will fail.
		if err := targetFile.Sync(); err != nil {
			return diag.Errorf("sync target optical disk image to filesystem: %v", err)
		}

		if err := targetFile.Close(); err != nil {
			return diag.Errorf("close target optical disk image: %v", err)
		}

		if err := vm.AttachStorage("SATA", vbox.StorageMedium{
			Port:      uint(len(vmDisks) + i),
			Device:    0,
			DriveType: vbox.DriveDVD,
			Medium:    target,
		}); err != nil {
			return diag.Errorf("unable to attach VirtualBox storage medium: %v", err)
		}
	}

	// Setup VM general properties
	if err := tfToVbox(ctx, d, vm); err != nil {
		return diag.Errorf("unable to convert Terraform data to VM properties: %v", err)
	}
	if err := vm.Modify(); err != nil {
		return diag.Errorf("can't set up VM properties: %v (verify ostype via `VBoxManage list ostypes`)", err)
	}

	// Start the VM
	if err := vm.Start(); err != nil {
		return diag.Errorf("unable to start VM: %v", err)
	}

	// Assign VM ID
	tflog.Debug(ctx, "resource ID", map[string]any{
		"uuid": vm.UUID,
	})
	d.SetId(vm.UUID)

	if err := waitUntilVMIsReady(ctx, d, vm, meta); err != nil {
		return diag.Errorf("failed to wait until VM is ready: %v", err)
	}

	// Errors here are already logged.
	return resourceVMRead(ctx, d, meta)
}

func setState(d *schema.ResourceData, state vbox.MachineState) error {
	var err error
	switch state {
	case vbox.Poweroff:
		err = d.Set("status", "poweroff")
	case vbox.Running:
		err = d.Set("status", "running")
	case vbox.Paused:
		err = d.Set("status", "paused")
	case vbox.Saved:
		err = d.Set("status", "saved")
	case vbox.Aborted:
		err = d.Set("status", "aborted")
	}
	if err != nil {
		return fmt.Errorf("unable to update VM state: %w", err)
	}
	return nil
}

func resourceVMRead(ctx context.Context, d *schema.ResourceData, meta any) diag.Diagnostics {
	vm, err := vbox.GetMachine(d.Id())
	switch err {
	case nil:
		break
	case vbox.ErrMachineNotExist:
		// VM no longer exists.
		d.SetId("")
		return nil
	default:
		return diag.Errorf("unable to get machine: %v", err)
	}

	// if vm.State != vbox.Running {
	//	setState(d, vm.State)
	//	return nil
	// }

	err = setState(d, vm.State)
	if err != nil {
		return diag.Errorf("can't set state: %v", err)
	}
	err = d.Set("name", vm.Name)
	if err != nil {
		return diag.Errorf("can't set name: %v", err)
	}
	err = d.Set("cpus", vm.CPUs)
	if err != nil {
		return diag.Errorf("can't set cpus: %v", err)
	}
	// Always store memory as MiB to avoid phantom diffs from format differences
	// (e.g., "1.0 gib" vs "1024 mib" are the same value but different strings)
	err = d.Set("memory", fmt.Sprintf("%d mib", vm.Memory))
	if err != nil {
		return diag.Errorf("can't set memory: %v", err)
	}

	if err = netVboxToTf(vm, d); err != nil {
		return diag.Errorf("can't convert vbox network to terraform data: %v", err)
	}

	/* Set connection info to first non NAT IPv4 address */
	for i, nic := range vm.NICs {
		if nic.Network == vbox.NICNetNAT {
			continue
		}
		availKey := fmt.Sprintf("network_adapter.%d.ipv4_address_available", i)
		if d.Get(availKey).(string) != "yes" {
			continue
		}
		ipv4Key := fmt.Sprintf("network_adapter.%d.ipv4_address", i)
		ipv4 := d.Get(ipv4Key).(string)
		if ipv4 == "" {
			continue
		}
		d.SetConnInfo(map[string]string{
			"type": "ssh",
			"host": ipv4,
		})
		break
	}

	err = d.Set("boot_order", vm.BootOrder)
	if err != nil {
		return diag.Errorf("can't set boot_order: %v", err)
	}

	// Set disk_size from primary disk for drift detection (only when disk_size was configured)
	if _, ok := d.GetOk("disk_size"); ok {
		vmDisks, err := gatherDisks(vm.BaseFolder)
		if err == nil && len(vmDisks) > 0 {
			for _, diskPath := range vmDisks {
				if strings.Contains(strings.ToLower(filepath.Base(diskPath)), "configdrive") {
					continue
				}
				sizeMiB, err := getDiskSizeMiB(ctx, diskPath)
				if err == nil {
					if err := d.Set("disk_size", fmt.Sprintf("%d mib", sizeMiB)); err != nil {
						return diag.Errorf("can't set disk_size: %v", err)
					}
					break
				}
			}
		}
	}

	return nil
}

func powerOnAndWait(ctx context.Context, d *schema.ResourceData, vm *vbox.Machine, meta any) error {
	if err := vm.Start(); err != nil {
		return fmt.Errorf("can't start vm: %w", err)
	}

	if err := waitUntilVMIsReady(ctx, d, vm, meta); err != nil {
		return fmt.Errorf("unabke to poer on and wait: %w", err)
	}

	return nil
}

func resourceVMUpdate(ctx context.Context, d *schema.ResourceData, meta any) diag.Diagnostics {
	// Skip update if no modifiable attributes changed (avoids unnecessary poweroff/modify cycles)
	if !d.HasChanges("cpus", "memory", "ostype", "network_adapter", "boot_order", "optical_disks", "status", "disk_size") {
		tflog.Debug(ctx, "no modifiable attributes changed, skipping VM update")
		return resourceVMRead(ctx, d, meta)
	}

	vm, err := vbox.GetMachine(d.Id())
	if err != nil {
		return diag.Errorf("unable to get machine %s: %v", d.Id(), err)
	}

	if err := vm.Poweroff(); err != nil {
		return diag.Errorf("unable to poweroff machine %s: %v", d.Id(), err)
	}

	// Brief pause to allow VBoxManage to fully release locks after poweroff
	time.Sleep(1 * time.Second)

	// Resize disks if disk_size changed
	if d.HasChange("disk_size") {
		if diskSizeStr, ok := d.GetOk("disk_size"); ok {
			clonedDisks, err := gatherDisks(vm.BaseFolder)
			if err != nil {
				return diag.Errorf("unable to gather disks for resizing: %v", err)
			}
			for i, disk := range clonedDisks {
				if strings.Contains(strings.ToLower(filepath.Base(disk)), "configdrive") {
					continue
				}
				resized, err := resizeDisk(ctx, disk, diskSizeStr.(string))
				if err != nil {
					return diag.Errorf("failed to resize disk %s: %v", disk, err)
				}
				// If the disk was converted (VMDK -> VDI), remove the old VMDK and update storage attachment
				if resized != disk {
					if err := os.Remove(disk); err != nil {
						tflog.Warn(ctx, "failed to remove old VMDK after conversion", map[string]any{
							"disk":  disk,
							"error": err.Error(),
						})
					}
					// Update VM storage attachment to point to new VDI
					_, stderr, err := vbox.Run(ctx, "storageattach", vm.Name, "--storagectl", "SATA", "--port", strconv.Itoa(i), "--medium", resized)
					if err != nil {
						return diag.Errorf("failed to update storage attachment after VMDK conversion: %v (stderr: %s)", err, stderr)
					}
				}
			}
		}
	}

	// Modify VM
	if err := tfToVbox(ctx, d, vm); err != nil {
		return diag.Errorf("can't convert terraform config to virtual machine: %v", err)
	}
	if err := modifyVM(ctx, vm); err != nil {
		return diag.Errorf("unable to modify the vm: %v", err)
	}

	if err := powerOnAndWait(ctx, d, vm, meta); err != nil {
		return diag.Errorf("unable to power on and wait for VM: %v", err)
	}

	// Errors are already logged
	return resourceVMRead(ctx, d, meta)
}

// modifyVM runs VBoxManage modifyvm directly (instead of the library's vm.Modify())
// so we can capture stderr and provide meaningful error messages.
func modifyVM(ctx context.Context, vm *vbox.Machine) error {
	args := []string{"modifyvm", vm.Name,
		"--firmware", vm.Firmware,
		"--bioslogofadein", "off",
		"--bioslogofadeout", "off",
		"--bioslogodisplaytime", "0",
		"--biosbootmenu", "disabled",
		"--ostype", vm.OSType,
		"--cpus", fmt.Sprintf("%d", vm.CPUs),
		"--memory", fmt.Sprintf("%d", vm.Memory),
		"--vram", fmt.Sprintf("%d", vm.VRAM),
		"--acpi", vm.Flag.Get(vbox.ACPI),
		"--ioapic", vm.Flag.Get(vbox.IOAPIC),
		"--rtcuseutc", vm.Flag.Get(vbox.RTCUSEUTC),
		"--cpuhotplug", vm.Flag.Get(vbox.CPUHOTPLUG),
		"--pae", vm.Flag.Get(vbox.PAE),
		"--longmode", vm.Flag.Get(vbox.LONGMODE),
		"--hpet", vm.Flag.Get(vbox.HPET),
		"--hwvirtex", vm.Flag.Get(vbox.HWVIRTEX),
		"--triplefaultreset", vm.Flag.Get(vbox.TRIPLEFAULTRESET),
		"--nestedpaging", vm.Flag.Get(vbox.NESTEDPAGING),
		"--largepages", vm.Flag.Get(vbox.LARGEPAGES),
		"--vtxvpid", vm.Flag.Get(vbox.VTXVPID),
		"--vtxux", vm.Flag.Get(vbox.VTXUX),
		"--accelerate3d", vm.Flag.Get(vbox.ACCELERATE3D),
	}

	for i, dev := range vm.BootOrder {
		if i > 3 {
			break
		}
		args = append(args, fmt.Sprintf("--boot%d", i+1), dev)
	}

	for i, nic := range vm.NICs {
		n := i + 1
		args = append(args,
			fmt.Sprintf("--nic%d", n), string(nic.Network),
			fmt.Sprintf("--nictype%d", n), string(nic.Hardware),
			fmt.Sprintf("--cableconnected%d", n), "on")
		if nic.Network == vbox.NICNetHostonly {
			args = append(args, fmt.Sprintf("--hostonlyadapter%d", n), nic.HostInterface)
		} else if nic.Network == vbox.NICNetBridged {
			args = append(args, fmt.Sprintf("--bridgeadapter%d", n), nic.HostInterface)
		}
	}

	tflog.Debug(ctx, "running modifyvm", map[string]any{"args": args})
	_, stderr, err := vbox.Run(ctx, args...)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail != "" {
			return fmt.Errorf("%v: %s", err, detail)
		}
		return fmt.Errorf("%v (verify ostype via `VBoxManage list ostypes`)", err)
	}

	return vm.Refresh()
}

func resourceVMDelete(d *schema.ResourceData, meta any) error {
	vm, err := vbox.GetMachine(d.Id())
	if err != nil {
		return fmt.Errorf("unable to get machine for deletion: %w", err)
	}
	if err := vm.Delete(); err != nil {
		return fmt.Errorf("unabke to remove the VM: %w", err)
	}
	return nil
}

// Wait until VM is ready, and 'ready' means the first non NAT NIC get a ipv4_address assigned
func waitUntilVMIsReady(ctx context.Context, d *schema.ResourceData, vm *vbox.Machine, meta any) error {
	for i, nic := range vm.NICs {
		if nic.Network == vbox.NICNetNAT {
			continue
		}

		key := fmt.Sprintf("network_adapter.%d.ipv4_address_available", i)
		if _, err := waitForVMAttribute(
			ctx,
			d,
			[]string{"yes"},
			[]string{"no"},
			key,
			meta,
			30*time.Second,
			1*time.Second,
		); err != nil {
			return fmt.Errorf("waiting for VM (%s) to become ready: %w", d.Get("name"), err)
		}
		break
	}
	return nil
}

func tfToVbox(ctx context.Context, d *schema.ResourceData, vm *vbox.Machine) error {
	var err error

	vm.OSType = d.Get("ostype").(string)
	vm.CPUs = uint(d.Get("cpus").(int))
	bytes, err := humanize.ParseBytes(d.Get("memory").(string))
	if err != nil {
		return fmt.Errorf("cannot humanize bytes: %w", err)
	}
	vm.Memory = uint(bytes / humanize.MiByte) // VirtualBox expect memory to be in MiB units

	vm.VRAM = 20 // Always 10MiB for vram
	vm.Flag = vbox.ACPI | vbox.IOAPIC | vbox.RTCUSEUTC | vbox.PAE |
		vbox.HWVIRTEX | vbox.NESTEDPAGING | vbox.LARGEPAGES | vbox.LONGMODE |
		vbox.VTXVPID | vbox.VTXUX
	vm.NICs, err = netTfToVbox(ctx, d)
	vm.BootOrder = defaultBootOrder
	for i, bootDev := range d.Get("boot_order").([]any) {
		vm.BootOrder[i] = bootDev.(string)
	}
	return err
}

func netTfToVbox(ctx context.Context, d *schema.ResourceData) ([]vbox.NIC, error) {
	tfToVboxNetworkType := func(attr string) (vbox.NICNetwork, error) {
		switch attr {
		case "bridged":
			return vbox.NICNetBridged, nil
		case "nat":
			return vbox.NICNetNAT, nil
		case "hostonly":
			return vbox.NICNetHostonly, nil
		case "internal":
			return vbox.NICNetInternal, nil
		case "generic":
			return vbox.NICNetGeneric, nil
		default:
			return "", fmt.Errorf("Invalid virtual network adapter type: %s", attr)
		}
	}

	tfToVboxNetDevice := func(attr string) (vbox.NICHardware, error) {
		switch attr {
		case "PCIII":
			return vbox.AMDPCNetPCIII, nil
		case "FASTIII":
			return vbox.AMDPCNetFASTIII, nil
		case "IntelPro1000MTDesktop":
			return vbox.IntelPro1000MTDesktop, nil
		case "IntelPro1000TServer":
			return vbox.IntelPro1000TServer, nil
		case "IntelPro1000MTServer":
			return vbox.IntelPro1000MTServer, nil
		case "VirtIO":
			return vbox.VirtIO, nil
		default:
			return "", fmt.Errorf("Invalid virtual network device: %s", attr)
		}
	}

	var err error
	var errs []error
	nicCount := d.Get("network_adapter.#").(int)
	adapters := make([]vbox.NIC, 0, nicCount)

	for i := 0; i < nicCount; i++ {
		prefix := fmt.Sprintf("network_adapter.%d.", i)
		var adapter vbox.NIC

		if attr, ok := d.Get(prefix + "type").(string); ok && attr != "" {
			adapter.Network, err = tfToVboxNetworkType(attr)
		}
		if attr, ok := d.Get(prefix + "device").(string); ok && attr != "" {
			adapter.Hardware, err = tfToVboxNetDevice(attr)
		}
		/* 'Hostonly' and 'bridged' network need property 'host_interface' been set */
		if adapter.Network == vbox.NICNetHostonly || adapter.Network == vbox.NICNetBridged {
			var ok bool
			adapter.HostInterface, ok = d.Get(prefix + "host_interface").(string)
			if !ok || adapter.HostInterface == "" {
				err = fmt.Errorf("'host_interface' property not set for '#%d' network adapter", i)
			}
		}

		if err != nil {
			errs = append(errs, err)
			continue
		}

		tflog.Debug(ctx, "adding new converted network adapter", map[string]any{
			"adapter": fmt.Sprintf("%+v", adapter),
		})
		adapters = append(adapters, adapter)
	}

	if len(errs) > 0 {
		return nil, &multierror.Error{Errors: errs}
	}

	return adapters, nil
}

// countRuntimeNics will return the number of NICs found after VM successfully started.
func countRuntimeNICs(vm *vbox.Machine) (int, error) {
	count, err := vbox.GetGuestProperty(vm.UUID, "/VirtualBox/GuestInfo/Net/Count")

	if err != nil {
		return 0, err
	}

	if count == "" {
		return 0, nil
	}

	return strconv.Atoi(count)
}

func netVboxToTf(vm *vbox.Machine, d *schema.ResourceData) error {
	vboxToTfNetworkType := func(netType vbox.NICNetwork) string {
		switch netType {
		case vbox.NICNetBridged:
			return "bridged"
		case vbox.NICNetNAT:
			return "nat"
		case vbox.NICNetHostonly:
			return "hostonly"
		case vbox.NICNetInternal:
			return "internal"
		case vbox.NICNetGeneric:
			return "generic"
		default:
			return ""
		}
	}

	vboxToTfVdevice := func(vdevice vbox.NICHardware) string {
		switch vdevice {
		case vbox.AMDPCNetPCIII:
			return "PCIII"
		case vbox.AMDPCNetFASTIII:
			return "FASTIII"
		case vbox.IntelPro1000MTDesktop:
			return "IntelPro1000MTDesktop"
		case vbox.IntelPro1000TServer:
			return "IntelPro1000TServer"
		case vbox.IntelPro1000MTServer:
			return "IntelPro1000MTServer"
		case vbox.VirtIO:
			return "VirtIO"
		default:
			return ""
		}
	}

	/* Collect NIC data from guest OS, available only when VM is running */
	if vm.State == vbox.Running {
		nicCount, err := countRuntimeNICs(vm)
		if err != nil {
			return err
		}

		if nicCount < len(vm.NICs) {
			return nil
		}

		/* NICs in guest OS (eth0, eth1, etc) does not neccessarily have save
		order as in VirtualBox (nic1, nic2, etc), so we use MAC address to setup a mapping */
		type OsNicData struct {
			ipv4Addr string
			status   string
		}
		osNicMap := make(map[string]OsNicData) // map from MAC address to data

		var errs []error
		for i := 0; i < nicCount; i++ {
			var osNic OsNicData

			/* NIC MAC address */
			macAddr, err := vbox.GetGuestProperty(vm.UUID, fmt.Sprintf("/VirtualBox/GuestInfo/Net/%d/MAC", i))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if macAddr == "" {
				return nil
			}

			/* NIC status */
			status, err := vbox.GetGuestProperty(vm.UUID, fmt.Sprintf("/VirtualBox/GuestInfo/Net/%d/Status", i))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if status == "" {
				return nil
			}
			osNic.status = strings.ToLower(status)

			/* NIC ipv4 address */
			ipv4Addr, err := vbox.GetGuestProperty(vm.UUID, fmt.Sprintf("/VirtualBox/GuestInfo/Net/%d/V4/IP", i))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ipv4Addr == "" {
				return nil
			}
			osNic.ipv4Addr = ipv4Addr

			osNicMap[macAddr] = osNic
		}

		if len(errs) > 0 {
			return &multierror.Error{Errors: errs}
		}

		// Assign NIC property to vbox structure and Terraform
		nics := make([]map[string]any, 0, 1)

		for _, nic := range vm.NICs {
			out := make(map[string]any)

			out["type"] = vboxToTfNetworkType(nic.Network)
			out["device"] = vboxToTfVdevice(nic.Hardware)
			out["host_interface"] = nic.HostInterface
			out["mac_address"] = nic.MacAddr

			osNic, ok := osNicMap[nic.MacAddr]
			if !ok {
				return nil
			}
			out["status"] = osNic.status
			out["ipv4_address"] = osNic.ipv4Addr
			// Only consider non-APIPA addresses as available
			if isValidIPAddress(osNic.ipv4Addr) {
				out["ipv4_address_available"] = "yes"
			} else {
				out["ipv4_address_available"] = "no"
			}

			nics = append(nics, out)
		}

		err = d.Set("network_adapter", nics)
		if err != nil {
			return fmt.Errorf("can't set network_adapter: %w", err)
		}

	} else {
		// Assign NIC property to vbox structure and Terraform
		nics := make([]map[string]any, 0, 1)

		for _, nic := range vm.NICs {
			out := make(map[string]any)

			out["type"] = vboxToTfNetworkType(nic.Network)
			out["device"] = vboxToTfVdevice(nic.Hardware)
			out["host_interface"] = nic.HostInterface
			out["mac_address"] = nic.MacAddr

			out["status"] = "down"
			out["ipv4_address"] = ""
			out["ipv4_address_available"] = "no"

			nics = append(nics, out)
		}

		if err := d.Set("network_adapter", nics); err != nil {
			return fmt.Errorf("can't set network_adapter: %w", err)
		}

	}

	return nil
}

func waitForVMAttribute(ctx context.Context, d *schema.ResourceData, target []string, pending []string, attribute string, meta any, delay, interval time.Duration) (any, error) {
	// Wait for the vm so we can get the networking attributes that show up
	// after a while.
	tflog.Debug(ctx, "waiting for vm to have required attribute value", map[string]any{
		"vm":        d.Get("name"),
		"attribute": attribute,
		"target":    "target",
	})

	stateConf := &resource.StateChangeConf{
		Pending:        pending,
		Target:         target,
		Refresh:        newVMStateRefreshFunc(ctx, d, attribute, meta),
		Timeout:        30 * time.Minute,
		Delay:          delay,
		MinTimeout:     interval,
		NotFoundChecks: 1800,
	}

	return stateConf.WaitForStateContext(ctx)
}

func newVMStateRefreshFunc(ctx context.Context, d *schema.ResourceData, attribute string, meta any) resource.StateRefreshFunc {
	return func() (any, string, error) {
		diags := resourceVMRead(ctx, d, meta)
		if diags.HasError() {
			// During VM startup, guest additions may not be ready yet to provide properties.
			// Treat this as a pending state rather than fatal error - let timeout handle true failures.
			tflog.Debug(ctx, "VM read failed during wait, treating as pending", map[string]any{
				"diagnostics": diags,
			})
			// Return a non-nil placeholder so this doesn't count as "not found"
			return "pending", "no", nil
		}

		// See if we can access our attribute
		if attr, ok := d.GetOk(attribute); ok {
			// Retrieve the VM properties
			vm, err := vbox.GetMachine(d.Id())
			if err != nil {
				// VM exists but can't retrieve properties - still starting up
				tflog.Debug(ctx, "VM properties unavailable, treating as pending", map[string]any{
					"error": err.Error(),
				})
				return "pending", "no", nil
			}

			return &vm, attr.(string), nil
		}

		// Return "no" as the state when attribute doesn't exist yet
		// This matches the pending state and allows proper state transitions
		return "pending", "no", nil
	}
}

// getDiskSizeMiB queries VBoxManage for the current logical size of a disk in MiB.
func getDiskSizeMiB(ctx context.Context, diskPath string) (uint64, error) {
	stdout, stderr, err := vbox.Run(ctx, "showmediuminfo", "disk", diskPath)
	if err != nil {
		return 0, fmt.Errorf("failed to get medium info for %s: %w (stderr: %s)", diskPath, err, stderr)
	}
	// Parse "Capacity:       40960 MBytes" from showmediuminfo output
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Capacity:") {
			parts := strings.Fields(line)
			// Expect: ["Capacity:", "<number>", "MBytes"]
			if len(parts) >= 2 {
				size, err := strconv.ParseUint(parts[1], 10, 64)
				if err != nil {
					return 0, fmt.Errorf("cannot parse disk capacity %q: %w", parts[1], err)
				}
				return size, nil
			}
		}
	}
	return 0, fmt.Errorf("could not find Capacity in showmediuminfo output for %s", diskPath)
}

// resizeDisk resizes a virtual disk to the specified size.
// VBoxManage modifymedium only supports VDI and VHD formats.
// If the disk is a VMDK, it will be cloned to VDI format first, then resized.
// Returns the final disk path (which may differ from input if format conversion occurred).
func resizeDisk(ctx context.Context, diskPath string, sizeStr string) (string, error) {
	sizeBytes, err := humanize.ParseBytes(sizeStr)
	if err != nil {
		return "", fmt.Errorf("cannot parse disk_size %q: %w", sizeStr, err)
	}
	// VBoxManage --resize expects size in MiB (binary megabytes)
	sizeMiB := sizeBytes / humanize.MiByte
	if sizeMiB == 0 {
		return "", fmt.Errorf("disk_size %q is too small (must be at least 1 MiB)", sizeStr)
	}

	finalPath := diskPath
	ext := strings.ToLower(filepath.Ext(diskPath))

	// VMDK format does not support direct resize; convert to VDI first
	if ext == ".vmdk" {
		vdiPath := strings.TrimSuffix(diskPath, filepath.Ext(diskPath)) + ".vdi"
		tflog.Info(ctx, "converting VMDK to VDI for resize support", map[string]any{
			"source": diskPath,
			"target": vdiPath,
		})
		stdout, stderr, err := vbox.Run(ctx, "clonehd", diskPath, vdiPath, "--format", "VDI")
		if err != nil {
			return "", fmt.Errorf("failed to convert VMDK to VDI: %w (stdout: %s, stderr: %s)", err, stdout, stderr)
		}
		finalPath = vdiPath
	}

	// Check current disk size — VBoxManage cannot shrink, only grow
	currentMiB, err := getDiskSizeMiB(ctx, finalPath)
	if err != nil {
		tflog.Warn(ctx, "could not determine current disk size, attempting resize anyway", map[string]any{
			"disk":  finalPath,
			"error": err.Error(),
		})
	} else if sizeMiB <= currentMiB {
		if sizeMiB < currentMiB {
			return "", fmt.Errorf("shrinking is not supported: requested %d MiB is smaller than current %d MiB; use a larger size or recreate the VM", sizeMiB, currentMiB)
		}
		tflog.Info(ctx, "disk is already equal to requested size, skipping resize", map[string]any{
			"disk":          finalPath,
			"current_mib":   currentMiB,
			"requested_mib": sizeMiB,
		})
		return finalPath, nil
	}

	tflog.Info(ctx, "resizing disk", map[string]any{
		"disk":        finalPath,
		"current_mib": currentMiB,
		"target_mib":  sizeMiB,
	})
	stdout, stderr, err := vbox.Run(ctx, "modifymedium", "disk", finalPath, "--resize", fmt.Sprintf("%d", sizeMiB))
	if err != nil {
		return "", fmt.Errorf("failed to resize disk to %d MiB: %w (stdout: %s, stderr: %s)", sizeMiB, err, stdout, stderr)
	}

	return finalPath, nil
}

func fetchIfRemote(u *url.URL) (string, error) {
	// If the schema is empty, treat it as a local path, otherwise
	// use it as a remote.
	if u.Scheme == "" {
		return u.Path, nil
	}

	// TODO: Add special handing for other schemes, such as
	//		 s3, gcs, (s)ftp(s).
	// We want to quit if the scheme is not currently supported.
	switch u.Scheme {
	case "http", "https":
		break
	default:
		return "", fmt.Errorf("unsupported scheme %s", u.Scheme)
	}

	_, file := filepath.Split(u.Path)

	// if the file is not found, and the error is unexpected, return
	if _, err := os.Stat(file); err != nil && !os.IsNotExist(err) {
		return "", err
	}

	f, err := os.Create(file)
	if err != nil {
		return "", err
	}
	defer f.Close()

	resp, err := http.Get(u.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", err
	}

	return file, nil
}
