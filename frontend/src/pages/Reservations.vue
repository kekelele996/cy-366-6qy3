<template>
  <div class="reservations-page">
    <van-cell-group inset title="新增预约（提交后为待确认，需店员确认）">
      <van-field v-model="form.station_id" type="number" label="机位ID" placeholder="输入机位ID" />
      <van-field :model-value="form.start_time" label="开始时间" placeholder="如 2026-08-17 10:00:00" readonly is-link @click="openPicker('startCreate')" />
      <van-field :model-value="form.end_time" label="结束时间" placeholder="如 2026-08-17 12:00:00" readonly is-link @click="openPicker('endCreate')" />
      <van-field v-model="form.remark" label="备注" placeholder="选填" />
    </van-cell-group>
    <div class="submit-btn"><van-button round block type="primary" @click="create">提交预约</van-button></div>

    <van-dropdown-menu>
      <van-dropdown-item v-model="status" :options="statusOptions" @change="reload" />
    </van-dropdown-menu>
    <van-notice-bar left-icon="info-o" text="已确认预约仅可在开始前15分钟至结束前开机；逾期未开机系统自动取消" />
    <van-cell-group inset title="预约列表">
      <van-empty v-if="list.length === 0" description="暂无预约" />
      <van-cell v-for="r in list" :key="r.id" :title="`预约 #${r.id} · 机位 ${r.station_id}${isStaffOrAdmin ? ` · 会员 ${r.user_id}` : ''}`" :label="`${formatTime(r.start_time)} ~ ${formatTime(r.end_time)}`">
        <template #value>
          <StatusBadge kind="reservation" :status="r.status" />
          <van-button v-if="isStaffOrAdmin && r.status === 'pending'" size="mini" type="success" plain class="op-btn" @click="confirm(r)">确认</van-button>
          <van-button v-if="isStaffOrAdmin && r.status === 'confirmed'" size="mini" type="primary" class="op-btn" @click="checkIn(r)">开机</van-button>
          <van-button v-if="!isStaffOrAdmin && r.status === 'confirmed'" size="mini" type="primary" class="op-btn" @click="memberCheckIn(r)">到店开机</van-button>
          <van-button v-if="canReschedule(r)" size="mini" type="warning" plain class="op-btn" @click="openReschedule(r)">改约</van-button>
          <van-button v-if="canCancel(r)" size="mini" type="danger" plain class="op-btn" @click="cancel(r)">取消</van-button>
        </template>
      </van-cell>
    </van-cell-group>
    <van-pagination v-model="page" :total-items="total" :items-per-page="pageSize" @change="reload" />

    <!-- 改约弹层 -->
    <van-popup v-model:show="rescheduleVisible" position="bottom" round teleport="body">
      <van-cell-group inset title="改约（先释放原时段，再占用新时段）">
        <van-field v-model="rescheduleForm.station_id" type="number" label="新机位ID" placeholder="留空表示不换机" />
        <van-field :model-value="rescheduleForm.start_time" label="新开始" readonly is-link @click="openPicker('startRes')" />
        <van-field :model-value="rescheduleForm.end_time" label="新结束" readonly is-link @click="openPicker('endRes')" />
      </van-cell-group>
      <div class="picker-actions">
        <van-button block plain type="default" @click="rescheduleVisible = false">取消</van-button>
        <van-button block type="primary" @click="submitReschedule">确认改约</van-button>
      </div>
    </van-popup>

    <!-- 日期+时间选择弹层（创建/改约共用） -->
    <van-popup v-model:show="picker.show" position="bottom" round teleport="body">
      <van-date-picker v-model="picker.date" title="选择日期" :min-date="minDate" :max-date="maxDate" />
      <van-time-picker v-model="picker.time" title="选择时间" :columns-type="['hour', 'minute']" />
      <div class="picker-actions">
        <van-button block plain type="default" @click="picker.show = false">取消</van-button>
        <van-button block type="primary" @click="confirmPicker">确定</van-button>
      </div>
    </van-popup>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { showConfirmDialog, showSuccessToast, showToast } from 'vant'
import StatusBadge from '@/components/StatusBadge.vue'
import {
  listReservations,
  createReservation,
  confirmReservation,
  cancelReservation,
  checkInReservation,
  rescheduleReservation,
  type Reservation,
} from '@/api/reservation'
import { startSession } from '@/api/session'
import { formatTime } from '@/utils/format'
import { useAuth } from '@/hooks/useAuth'

const { isStaffOrAdmin, user } = useAuth()
const list = ref<Reservation[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = 10
const status = ref('')
const statusOptions = [
  { text: '全部状态', value: '' },
  { text: '待确认', value: 'pending' },
  { text: '已确认', value: 'confirmed' },
  { text: '已开机', value: 'checked_in' },
  { text: '已完成', value: 'completed' },
  { text: '已取消', value: 'cancelled' },
]
const form = reactive({ station_id: '', start_time: '', end_time: '', remark: '' })

type PickerTarget = 'startCreate' | 'endCreate' | 'startRes' | 'endRes'
const picker = reactive({
  show: false,
  target: 'startCreate' as PickerTarget,
  date: defaultDateParts(),
  time: ['10', '00'] as string[],
})
const minDate = new Date()
const maxDate = new Date(Date.now() + 1000 * 60 * 60 * 24 * 60)

const rescheduleVisible = ref(false)
const rescheduleForm = reactive<{ id: number; station_id: string; start_time: string; end_time: string }>({
  id: 0,
  station_id: '',
  start_time: '',
  end_time: '',
})

function defaultDateParts(): string[] {
  const d = new Date()
  return [String(d.getFullYear()), String(d.getMonth() + 1).padStart(2, '0'), String(d.getDate()).padStart(2, '0')]
}

function partsOf(value: string): { date: string[]; time: string[] } {
  // 兼容 RFC3339 与本地时间两种格式。
  const normalized = value.includes('T') ? value.replace('T', ' ').replace(/([+-]\d{2}:\d{2}|Z)$/, '') : value
  const [datePart, timePart = '10:00:00'] = normalized.split(' ')
  const [hh = '10', mm = '00'] = timePart.split(':')
  return { date: datePart.split('-'), time: [hh, mm] }
}

function openPicker(target: PickerTarget) {
  picker.target = target
  const source =
    target === 'startCreate' ? form.start_time :
    target === 'endCreate' ? form.end_time :
    target === 'startRes' ? rescheduleForm.start_time :
    rescheduleForm.end_time
  if (source) {
    const p = partsOf(source)
    picker.date = p.date
    picker.time = p.time
  } else {
    picker.date = defaultDateParts()
    picker.time = target.startsWith('start') ? ['10', '00'] : ['12', '00']
  }
  picker.show = true
}

function confirmPicker() {
  const stamp = `${picker.date.join('-')} ${picker.time[0].padStart(2, '0')}:${picker.time[1].padStart(2, '0')}:00`
  switch (picker.target) {
    case 'startCreate': form.start_time = stamp; break
    case 'endCreate': form.end_time = stamp; break
    case 'startRes': rescheduleForm.start_time = stamp; break
    case 'endRes': rescheduleForm.end_time = stamp; break
  }
  picker.show = false
}

async function reload() {
  const data = await listReservations({ page: page.value, page_size: pageSize, status: status.value || undefined })
  list.value = data.list
  total.value = data.total
}

async function create() {
  const stationId = Number(form.station_id)
  if (!stationId || !form.start_time || !form.end_time) {
    showToast('请填写机位ID与起止时间')
    return
  }
  await createReservation({ station_id: stationId, start_time: form.start_time, end_time: form.end_time, remark: form.remark })
  showSuccessToast('预约已提交，等待店员确认')
  form.station_id = ''
  form.start_time = ''
  form.end_time = ''
  form.remark = ''
  reload()
}

function canReschedule(r: Reservation) {
  if (!['pending', 'confirmed'].includes(r.status)) return false
  return isStaffOrAdmin.value || r.user_id === user.value?.id
}

function canCancel(r: Reservation) {
  if (isStaffOrAdmin.value) return ['pending', 'confirmed', 'checked_in'].includes(r.status)
  return r.user_id === user.value?.id && ['pending', 'confirmed'].includes(r.status)
}

async function confirm(r: Reservation) {
  await showConfirmDialog({ title: '确认预约', message: `确认预约 #${r.id}？` })
  await confirmReservation(r.id)
  showSuccessToast('已确认')
  reload()
}

async function checkIn(r: Reservation) {
  await checkInReservation(r.id)
  showSuccessToast('开机成功')
  reload()
}

async function memberCheckIn(r: Reservation) {
  // 会员端走 /sessions 凭预约开机，与店员端最终状态一致（预约 checked_in + 机位 using）。
  const s = await startSession({ station_id: r.station_id, reservation_id: r.id })
  showSuccessToast(`开机成功，上机记录 #${s.id}`)
  reload()
}

function openReschedule(r: Reservation) {
  rescheduleForm.id = r.id
  rescheduleForm.station_id = String(r.station_id)
  rescheduleForm.start_time = r.start_time
  rescheduleForm.end_time = r.end_time
  rescheduleVisible.value = true
}

async function submitReschedule() {
  const stationId = Number(rescheduleForm.station_id)
  if (!rescheduleForm.start_time || !rescheduleForm.end_time) {
    showToast('请选择新的起止时间')
    return
  }
  await rescheduleReservation(rescheduleForm.id, {
    station_id: stationId || undefined,
    start_time: rescheduleForm.start_time,
    end_time: rescheduleForm.end_time,
  })
  showSuccessToast('改约成功，等待店员重新确认')
  rescheduleVisible.value = false
  reload()
}

async function cancel(r: Reservation) {
  await showConfirmDialog({ title: '取消预约', message: `确定取消预约 #${r.id}？原时段将立即释放` })
  await cancelReservation(r.id)
  showSuccessToast('已取消')
  reload()
}

onMounted(reload)
</script>

<style scoped>
.submit-btn { margin: 12px 16px; }
.op-btn { margin-left: 6px; }
.picker-actions { display: flex; gap: 8px; padding: 8px 16px 16px; }
</style>
