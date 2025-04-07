package populator

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/kubev2v/forklift/cmd/vsphere-xcopy-volume-populator/internal/vmware"
	"k8s.io/klog/v2"
)

//const xcopyInitiatorGroup = "xcopy-esxs"

type EsxCli interface {
	ListVibs() ([]string, error)
	VmkfstoolsClone(sourceVMDKFile, targetLUN string) error
}

type RemoteEsxcliPopulator struct {
	VSphereClient vmware.Client
	StorageApi    StorageApi
}

func NewWithRemoteEsxcli(storageApi StorageApi, vsphereHostname, vsphereUsername, vspherePassword string) (Populator, error) {
	c, err := vmware.NewClient(vsphereHostname, vsphereUsername, vspherePassword)
	if err != nil {
		return nil, fmt.Errorf("failed to create vmware client: %w", err)
	}
	return &RemoteEsxcliPopulator{
		VSphereClient: c,
		StorageApi:    storageApi,
	}, nil

}

func (p *RemoteEsxcliPopulator) Populate(sourceVMDKFile string, volumeHandle string, progress chan int, quit chan error) (err error) {
	// isn't it better to not call close the channel from the caller?
	defer func() {
		if err != nil {
			quit <- err
		}
	}()
	vmDisk, err := ParseVmdkPath(sourceVMDKFile)
	if err != nil {
		return err
	}

	klog.Infof("Starting to populate using remote esxcli vmkfstools, source vmdk %s target LUN %s", sourceVMDKFile, volumeHandle)
	lun, err := p.StorageApi.ResolveVolumeHandleToLUN(volumeHandle)
	if err != nil {
		return err
	}

	originalInitiatorGroups, err := p.StorageApi.CurrentMappedGroups(lun, nil)
	if err != nil {
		return fmt.Errorf("failed to fetch the current initiator groups of the lun %s: %w", lun.Name, err)
	}
	klog.Infof("Current initiator groups the LUN %s is mapped to %+v", lun.IQN, originalInitiatorGroups)

	//targetLUN := fmt.Sprintf("/vmfs/devices/disks/naa.%s%s", lun.ProviderID, lun.SerialNumber)
	//klog.Infof("resolved lun serial number %s with IQN %s to lun %s", lun.SerialNumber, lun.IQN, targetLUN)

	host, err := p.VSphereClient.GetEsxByVm(context.Background(), vmDisk.VMName)
	if err != nil {
		return err
	}
	klog.Infof("Working with ESXi %+v", host)

	// for iSCSI add the host to the group using IQN. Is there something else for FC?
	r, err := p.VSphereClient.RunEsxCommand(context.Background(), host, []string{"storage", "core", "adapter", "list"})
	if err != nil {
		return err
	}
	uniqueUIDs := make(map[string]bool)
	hbaUIDs := []string{}

	// Print the adapter information for debugging
	for i, val := range r {
		klog.Infof("Adapter [%d]: %+v", i, val)
		for key, field := range val {
			klog.Infof("  %s: %v", key, field)
		}
	}

	for _, a := range r {
		driver, hasDriver := a["Driver"]
		linkState, hasLink := a["LinkState"]
		uid, hasUID := a["UID"]

		if !hasDriver || !hasLink || !hasUID || len(driver) == 0 || len(linkState) == 0 || len(uid) == 0 {
			continue
		}

		drv := driver[0]
		link := linkState[0]
		id := uid[0]

		// Check if the UID is FC, iSCSI or NVMe-oF
		isTargetUID := strings.HasPrefix(id, "fc.") || strings.HasPrefix(id, "iqn.") || strings.HasPrefix(id, "nqn.")

		if link == "link-up" && isTargetUID {
			if _, exists := uniqueUIDs[id]; !exists {
				uniqueUIDs[id] = true
				hbaUIDs = append(hbaUIDs, id)
				klog.Infof("Storage Adapter UID: %s (Driver: %s)", id, drv)
			}
		}
	}

	xcopyInitiatorGroup := []string{}

	m, err := p.StorageApi.EnsureClonnerIgroup(xcopyInitiatorGroup, hbaUIDs)
	if err != nil {
		return fmt.Errorf("failed to add the ESX IQN %s to the initiator group %w", hbaUIDs, err)
	}

	err = p.StorageApi.Map(xcopyInitiatorGroup, lun, m)
	if err != nil {
		return fmt.Errorf("failed to map lun %s to initiator group %s: %w", lun, xcopyInitiatorGroup, err)
	}
	defer func() {
		fmt.Println("Unmapping before returning")
		p.StorageApi.UnMap(xcopyInitiatorGroup, lun, m)
		p.StorageApi.Map(originalInitiatorGroups, lun, m)
		fmt.Println("Remapping original initiator groups")
		//		for _, group := range originalInitiatorGroups {
		//p.StorageApi.Map(group, lun)
		//			vantara.Map(group, lun)
		//		}
	}()

	lun = p.StorageApi.GetNaaID(lun)
	targetLUN := fmt.Sprintf("/vmfs/devices/disks/naa.%s%s", lun.ProviderID, lun.SerialNumber)
	klog.Infof("resolved lun serial number %s with IQN %s to lun %s", lun.SerialNumber, lun.IQN, targetLUN)

	retries := 3
	for i := 3; i > 0; i-- {
		_, err = p.VSphereClient.RunEsxCommand(context.Background(), host, []string{"storage", "core", "adapter", "rescan", "-a", "1"})
		if err != nil {
			klog.Errorf("failed to rescan adapters, probably in progress. Rerty %d/%d", i, retries)
			time.Sleep(time.Duration(rand.Intn(10)))
		} else {
			break
		}
	}
	if err != nil {
		return err
	}
	naa := fmt.Sprintf("naa.%s%s", lun.ProviderID, lun.SerialNumber)

	_, err = p.VSphereClient.RunEsxCommand(context.Background(), host, []string{"storage", "core", "device", "list", "-d", naa})
	if err != nil {
		return fmt.Errorf("failed to locate the target LUN %s. Check the LUN details and the host mapping response: %s", naa, err)
	}

	start := time.Now()
	r, err = p.VSphereClient.RunEsxCommand(context.Background(), host, []string{"vmkfstools", "clone", "-s", vmDisk.Path(), "-t", targetLUN})
	duration := time.Since(start)

	if err != nil {

		klog.Infof("error response from esxcli %+v", r)
		return err
	}

	klog.Infof("vmkfstools clone took %s", duration)

	response := ""
	klog.Info("respose from esxcli ", r)

	for _, l := range r {
		response += l.Value("message")
	}

	go func() {
		// TODO need to process the vmkfstools stderr(probably) and to write the
		// progress to a file, and then continuously read and report on the channel
		progress <- 100
		quit <- nil
	}()
	return nil
}
