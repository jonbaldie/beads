package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// mergeSlotCmd is the parent command for merge-slot operations
var mergeSlotCmd = &cobra.Command{
	Use:     "merge-slot",
	GroupID: "issues",
	Short:   "Manage merge-slot gates for serialized conflict resolution",
	Long: `Merge-slot gates serialize conflict resolution in the merge queue.

A merge slot is an exclusive access primitive: only one agent can hold it at a time.
This prevents "monkey knife fights" where multiple polecats race to resolve conflicts
and create cascading conflicts.

Each rig has one merge slot bead: <prefix>-merge-slot (labeled gt:slot).
The slot uses:
  - status=open: slot is available
  - status=in_progress: slot is held
  - metadata.holder: who currently holds the slot
  - metadata.waiters: priority-ordered queue of waiters

Examples:
  bd merge-slot create              # Create merge slot for current rig
  bd merge-slot check               # Check if slot is available
  bd merge-slot acquire             # Try to acquire the slot
  bd merge-slot release             # Release the slot`,
}

var mergeSlotCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a merge slot bead for the current rig",
	Long: `Create a merge slot bead for serialized conflict resolution.

The slot ID is automatically generated based on the beads prefix (e.g., gt-merge-slot).
The slot is created with status=open (available).`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runMergeSlotCreate,
}

var mergeSlotCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check merge slot availability",
	Long: `Check if the merge slot is available or held.

Returns:
  - available: slot can be acquired
  - held by <holder>: slot is currently held
  - not found: no merge slot exists for this rig`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runMergeSlotCheck,
}

var mergeSlotAcquireCmd = &cobra.Command{
	Use:   "acquire",
	Short: "Acquire the merge slot",
	Long: `Attempt to acquire the merge slot for exclusive access.

If the slot is available (status=open), it will be acquired:
  - status set to in_progress
  - holder set to the requester

If the slot is held (status=in_progress), the command fails unless
--wait is passed, which adds the requester to the waiters queue.

Use --holder to specify who is acquiring (default: BEADS_ACTOR env var).`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runMergeSlotAcquire,
}

var mergeSlotReleaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Release the merge slot",
	Long: `Release the merge slot after conflict resolution is complete.

Sets status back to open and clears the holder field.
If there are waiters, the highest-priority waiter should then acquire.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runMergeSlotRelease,
}

func init() {
	mergeSlotAcquireCmd.Flags().String("holder", "", "Who is acquiring the slot (default: BEADS_ACTOR)")
	mergeSlotAcquireCmd.Flags().Bool("wait", false, "Add to waiters list if slot is held")
	mergeSlotReleaseCmd.Flags().String("holder", "", "Who is releasing the slot (for verification)")

	mergeSlotCmd.AddCommand(mergeSlotCreateCmd)
	mergeSlotCmd.AddCommand(mergeSlotCheckCmd)
	mergeSlotCmd.AddCommand(mergeSlotAcquireCmd)
	mergeSlotCmd.AddCommand(mergeSlotReleaseCmd)
	rootCmd.AddCommand(mergeSlotCmd)
}

func runMergeSlotCreate(_ *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("merge-slot create is not supported in proxied-server mode")
	}
	if err := CheckReadonly("merge-slot create"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("merge-slot-create")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	issue, err := getStore().MergeSlotCreate(getRootContext(), getActor())
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	commandDidWrite.Store(true)

	if isJSONOutput() {
		result := map[string]interface{}{
			"id":     issue.ID,
			"status": string(issue.Status),
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}

	fmt.Printf("%s Created merge slot: %s\n", ui.RenderPass("✓"), issue.ID)
	return nil
}

func runMergeSlotCheck(_ *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("merge-slot check is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("merge-slot-check")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	status, err := getStore().MergeSlotCheck(getRootContext())
	if err != nil {
		if isNotFoundErr(err) {
			return renderMergeSlotNotFound()
		}
		return HandleErrorRespectJSON("%v", err)
	}
	return renderMergeSlotStatus(*status)
}

func renderMergeSlotNotFound() error {
	slotID := storage.MergeSlotID(getRootContext(), getStore())
	if isJSONOutput() {
		result := map[string]interface{}{
			"id":        slotID,
			"available": false,
			"error":     "not found",
		}
		return encodeMergeSlotJSON(result)
	}
	fmt.Printf("Merge slot not found: %s\n", slotID)
	fmt.Printf("Run 'bd merge-slot create' to create one.\n")
	return nil
}

func renderMergeSlotStatus(status storage.MergeSlotStatus) error {
	if isJSONOutput() {
		return encodeMergeSlotJSON(map[string]interface{}{
			"id":        status.SlotID,
			"available": status.Available,
			"holder":    nilIfEmpty(status.Holder),
			"waiters":   status.Waiters,
		})
	}
	if status.Available {
		fmt.Printf("%s Merge slot available: %s\n", ui.RenderPass("✓"), status.SlotID)
	} else {
		fmt.Printf("%s Merge slot held: %s\n", ui.RenderAccent("○"), status.SlotID)
		fmt.Printf("  Holder: %s\n", status.Holder)
		if len(status.Waiters) > 0 {
			fmt.Printf("  Waiters: %d\n", len(status.Waiters))
			for i, w := range status.Waiters {
				fmt.Printf("    %d. %s\n", i+1, w)
			}
		}
	}

	return nil
}

func encodeMergeSlotJSON(value interface{}) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func runMergeSlotAcquire(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("merge-slot acquire is not supported in proxied-server mode")
	}
	if err := CheckReadonly("merge-slot acquire"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("merge-slot-acquire")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	holder, addWaiter, err := mergeSlotAcquireOptions(cmd)
	if err != nil {
		return err
	}

	result, err := getStore().MergeSlotAcquire(getRootContext(), holder, getActor(), addWaiter)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return renderMergeSlotAcquireResult(*result, holder)
}

func renderMergeSlotAcquireResult(result storage.MergeSlotResult, holder string) error {
	if !result.Acquired && !result.Waiting {
		return renderMergeSlotBlocked(result)
	}

	if result.Waiting {
		return renderMergeSlotWaiting(result)
	}

	// Successfully acquired.
	commandDidWrite.Store(true)

	if isJSONOutput() {
		return encodeMergeSlotJSON(map[string]interface{}{
			"id":       result.SlotID,
			"acquired": true,
			"holder":   holder,
		})
	}

	fmt.Printf("%s Acquired merge slot: %s\n", ui.RenderPass("✓"), result.SlotID)
	fmt.Printf("  Holder: %s\n", holder)
	return nil
}

func mergeSlotAcquireOptions(cmd *cobra.Command) (string, bool, error) {
	holder, _ := cmd.Flags().GetString("holder")
	addWaiter, _ := cmd.Flags().GetBool("wait")
	if holder == "" {
		holder = getActor()
	}
	if holder == "" {
		return "", false, HandleError("no holder specified; use --holder or set BEADS_ACTOR env var")
	}
	return holder, addWaiter, nil
}

func renderMergeSlotBlocked(result storage.MergeSlotResult) error {
	if isJSONOutput() {
		if err := encodeMergeSlotJSON(map[string]interface{}{
			"id":       result.SlotID,
			"acquired": false,
			"holder":   result.Holder,
		}); err != nil {
			return err
		}
		return SilentExit()
	}
	return HandleErrorWithHint(fmt.Sprintf("slot held by: %s", result.Holder), "Use --wait to add yourself to the waiters queue.")
}

func renderMergeSlotWaiting(result storage.MergeSlotResult) error {
	if isJSONOutput() {
		if err := encodeMergeSlotJSON(map[string]interface{}{
			"id":       result.SlotID,
			"acquired": false,
			"waiting":  true,
			"holder":   result.Holder,
			"position": result.Position,
		}); err != nil {
			return err
		}
		return SilentExit()
	}
	fmt.Printf("%s Slot held by %s, added to waiters queue (position %d)\n",
		ui.RenderAccent("○"), result.Holder, result.Position)
	return SilentExit()
}

func runMergeSlotRelease(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("merge-slot release is not supported in proxied-server mode")
	}
	if err := CheckReadonly("merge-slot release"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("merge-slot-release")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	holder, _ := cmd.Flags().GetString("holder")
	if err := getStore().MergeSlotRelease(getRootContext(), holder, getActor()); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	commandDidWrite.Store(true)

	if isJSONOutput() {
		slotID := storage.MergeSlotID(getRootContext(), getStore())
		out := map[string]interface{}{
			"id":       slotID,
			"released": true,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(out)
	}

	slotID := storage.MergeSlotID(getRootContext(), getStore())
	fmt.Printf("%s Released merge slot: %s\n", ui.RenderPass("✓"), slotID)
	return nil
}

// nilIfEmpty returns nil if s is empty, otherwise returns s.
// Used for JSON output where empty strings should be null.
func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
