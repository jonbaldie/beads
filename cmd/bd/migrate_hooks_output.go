package main

func (p hookMigrationExecutionPlan) operationCount() int {
	return len(p.WriteOps) + len(p.RetireOps)
}

func (p hookMigrationExecutionPlan) outputOperations() []hookMigrationOutputOperation {
	ops := make([]hookMigrationOutputOperation, 0, p.operationCount())
	for _, write := range p.WriteOps {
		ops = append(ops, hookMigrationOutputOperation{
			Action:     "write_hook",
			HookName:   write.HookName,
			Path:       write.HookPath,
			SourcePath: write.SourcePath,
			State:      write.State,
		})
	}
	for _, retire := range p.RetireOps {
		ops = append(ops, hookMigrationOutputOperation{
			Action:      "retire_sidecar",
			HookName:    retire.HookName,
			Path:        retire.SourcePath,
			SourcePath:  retire.SourcePath,
			Destination: retire.DestinationPath,
		})
	}
	return ops
}
