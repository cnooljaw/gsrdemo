package main

import (
	"time"

	"github.com/cnooljaw/gsr"
)

const (
	maxMoleRefreshGroups = 128
	moleRefreshBudget    = 2 * time.Millisecond
)

func (service *moleService) addGroup(group *moleGroup) {
	service.groups[group.game.name] = group
	group.spawnElement = service.spawnOrder.PushBack(group)
}

func (service *moleService) removeGroup(group *moleGroup) {
	service.spawnOrder.Remove(group.spawnElement)
	group.spawnElement = nil
	delete(service.groups, group.game.name)
}

func (service *moleService) handleRefresh(self gsr.CommandContext) error {
	if self.Source() != self.Self() {
		return errInvalidMoleInput
	}
	if !service.refreshQueued {
		return nil
	}
	service.refreshQueued = false
	service.refreshGroups(self)
	return nil
}

// refreshGroups 每批有组数上限和软时间预算；单组完成后让出邮箱，不把预算当作抢占或硬超时。
func (service *moleService) refreshGroups(self gsr.ServiceContext) {
	deadline := time.Now().Add(moleRefreshBudget)
	for range maxMoleRefreshGroups {
		first := service.spawnOrder.Front()
		if first == nil {
			return
		}
		group, now := first.Value.(*moleGroup), time.Now()
		if now.Before(group.nextSpawnAt) {
			return
		}
		group.game.spawn(now)
		group.nextSpawnAt = now.Add(spawnPeriod)
		// 所有组的周期相同；新截止时间不早于尾部，移动到尾部即可保持到期顺序。
		service.spawnOrder.MoveToBack(first)
		service.broadcast(self, group)
		if !time.Now().Before(deadline) {
			break
		}
	}
	first := service.spawnOrder.Front()
	if first == nil || time.Now().Before(first.Value.(*moleGroup).nextSpawnAt) {
		return
	}
	if err := self.Send(self.Self(), moleRefreshCommand); err == nil {
		service.refreshQueued = true
	}
	// 未接纳时不留下虚假的在途标记，下一次 Pulse 会重新安排到期工作。
}
