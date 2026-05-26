package core

import (
	"fmt"

	panel "github.com/owo-space/sukad/api/sukashi"
)

func (v *Core) AddNode(tag string, info *panel.NodeInfo) error {
	if info.Type == "mieru" {
		v.access.Lock()
		defer v.access.Unlock()
		if _, ok := v.mieruNodes[tag]; ok {
			return nil
		}
		v.mieruNodes[tag] = newMieruNode(tag, info)
		return nil
	}
	if info.Type == "snell" {
		v.access.Lock()
		defer v.access.Unlock()
		if _, ok := v.snellNodes[tag]; ok {
			return nil
		}
		v.snellNodes[tag] = newSnellNode(tag, info)
		return nil
	}
	inBoundConfig, err := buildInbound(info, tag)
	if err != nil {
		return fmt.Errorf("build inbound error: %s", err)
	}
	err = v.addInbound(inBoundConfig)
	if err != nil {
		return fmt.Errorf("add inbound error: %s", err)
	}
	return nil
}

func (v *Core) DelNode(tag string) error {
	v.access.Lock()
	if node, ok := v.mieruNodes[tag]; ok {
		delete(v.mieruNodes, tag)
		v.access.Unlock()
		if err := node.close(); err != nil {
			return fmt.Errorf("close mieru node error: %s", err)
		}
		return nil
	}
	if node, ok := v.snellNodes[tag]; ok {
		delete(v.snellNodes, tag)
		v.access.Unlock()
		if err := node.close(); err != nil {
			return fmt.Errorf("close snell node error: %s", err)
		}
		return nil
	}
	v.access.Unlock()
	err := v.removeInbound(tag)
	if err != nil {
		return fmt.Errorf("remove in error: %s", err)
	}
	return nil
}
