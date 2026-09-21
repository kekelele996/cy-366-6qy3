<template>
  <div class="reservations-page">
    <van-cell-group inset title="新增预约">
      <van-field v-model="form.station_id" type="number" label="机位ID" placeholder="输入机位ID" />
      <van-field :model-value="form.start_time" label="开始时间" placeholder="如 2026-09-21 10:00:00" @click="openPicker('start')" readonly is-link />
      <van-field :model-value="form.end_time" label="结束时间" placeholder="如 2026-09-21 12:00:00" @click="openPicker('end')" readonly is-link />
      <van-field v-model="form.remark" label="备注" placeholder="选填" />
    </van-cell-group>
    <div class="submit-btn"><van-button round block type="primary" @click="create">提交预约</van-button></div>

    <van-dropdown-menu>
      <van-dropdown-item v-model="status" :options="statusOptions" @change="load" />
    </van-dropdown-menu>
    <van-notice-bar
      v-if="!isStaffOrAdmin"
      left-icon="info-o"
      text="仅展示本人的预约；提交后待店员确认，可在起始前 15 分钟至结束前到店开机，逾期将自动取消。"
    />
    <van-notice-bar
      v-else
      left-icon="manager-o"
      text="店员视图：展示全部会员预约，可确认、开机、改约或取消。"
    />
    <van-cell-group inset title="预约列表">
      <van-cell v-for="r in list" :key="r.id" :title="`预约 #${r.id} · 机位 ${r.station_id}`" :label="`${formatTime(r.start_time)} ~ ${formatTime(r.end_time)}`">
        <template #value>
          <div class="op-group">
            <StatusBadge kind="reservation" :status="r.status" />
            <van-button
              v-if="isStaffOrAdmin && r.status === 'pending'"
              size="mini" type="warning" plain class="op-btn"
              @click="confirm(r)"
            >确认</van-button>
            <van-button
              v-if="isStaffOrAdmin && r.status === 'confirmed'"
              size="mini" type="primary" class="op-btn"
              @click="checkIn(r)"
            >开机</van-button>
            <van-button
              v-if="canReschedule(r)"
              size="mini" type="primary" plain class="op-btn"
              @click="openReschedule(r)"
            >改约</van-button>
            <van-button
              v-if="canCancel(r)"
              size="mini" type="danger" plain class="op-btn"
              @click="cancel(r)"
            >取消</van-button>
          </div>
        </template>
      </van-cell>
      <EmptyState v-if="!loading && list.length === 0" description="暂无预约记录" />
    </van-cell-group>
    <van-pagination v-model="page" :total-items="total" :items-per-page="pageSize" @change="load" />

    <!-- 新增/改约共用：先选日期再选时分 -->
    <van-popup v-model:show="showDatePicker" position="bottom">
      <van-date-picker
        :model-value="pickerDate"
        :min-date="minDate"
        :max-date="maxDate"
        title="选择日期"
        @confirm="onDateConfirm"
        @cancel="showDatePicker = false"
      />
    </van-popup>
    <van-popup v-model:show="showTimePicker" position="bottom">
      <van-time-picker
        v-model="pickerTime"
        :show-toolbar="true"
        title="选择时间"
        @confirm="onTimeConfirm"
        @cancel="showTimePicker = false"
      />
    </van-popup>

    <!-- 改约弹窗：提交后服务端先释放原时段再占用新时段 -->
    <van-dialog
      v-model:show="showReschedule"
      title="改约"
      show-cancel-button
      :before-close="onRescheduleClose"
    >
      <van-cell-group>
        <van-field v-model="rescheduleForm.station_id" type="number" label="机位ID" placeholder="留空沿用原机位" />
        <van-field :model-value="rescheduleForm.start_time" label="新开始时间" readonly is-link @click="openReschedulePicker('start')" />
        <van-field :model-value="rescheduleForm.end_time" label="新结束时间" readonly is-link @click="openReschedulePicker('end')" />
        <van-field v-model="rescheduleForm.remark" label="备注" placeholder="选填" />
      </van-cell-group>
      <div class="reschedule-tip">先释放原时段再占用新时段；冲突或越权时原预约与机位状态保持不变。</div>
    </van-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { showSuccessToast, showToast, showConfirmDialog } from 'vant'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import {
  listReservations, createReservation, confirmReservation,
  cancelReservation, checkInReservation, rescheduleReservation,
  type Reservation,
} from '@/api/reservation'
import { formatTime } from '@/utils/format'
import { useAuth } from '@/hooks/useAuth'

const { isStaffOrAdmin, user } = useAuth()
const list = ref<Reservation[]>([])
const total = ref(0)
const loading = ref(false)
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
const form = ref({ station_id: '', start_time: '', end_time: '', remark: '' })

// 会员只能对本人的待确认/已确认预约执行取消、改约；店员/管理员可操作所有预约。
const ACTIVE = ['pending', 'confirmed']
function canCancel(r: Reservation) {
  if (!ACTIVE.includes(r.status)) return false
  return isStaffOrAdmin.value || r.user_id === user.value?.id
}
function canReschedule(r: Reservation) {
  if (!ACTIVE.includes(r.status)) return false
  return isStaffOrAdmin.value || r.user_id === user.value?.id
}

async function load() {
  loading.value = true
  try {
    const data = await listReservations({ page: page.value, page_size: pageSize, status: status.value || undefined })
    list.value = data.list
    total.value = data.total
  } finally {
    loading.value = false
  }
}

async function create() {
  const stationId = Number(form.value.station_id)
  if (!stationId || !form.value.start_time || !form.value.end_time) {
    showToast('请填写机位ID与起止时间')
    return
  }
  await createReservation({
    station_id: stationId,
    start_time: form.value.start_time,
    end_time: form.value.end_time,
    remark: form.value.remark,
  })
  showSuccessToast('预约成功，待店员确认')
  form.value = { station_id: '', start_time: '', end_time: '', remark: '' }
  load()
}

async function confirm(r: Reservation) {
  await confirmReservation(r.id)
  showSuccessToast('已确认')
  load()
}

async function cancel(r: Reservation) {
  try {
    await showConfirmDialog({ title: '取消预约', message: `确定取消预约 #${r.id} 吗？机位时段将被释放。` })
  } catch {
    return
  }
  await cancelReservation(r.id)
  showSuccessToast('已取消，机位时段已释放')
  load()
}

async function checkIn(r: Reservation) {
  await checkInReservation(r.id)
  showSuccessToast('开机成功')
  load()
}

/* ---------- 日期 + 时分两级选择器（新增与改约共用） ---------- */

const showDatePicker = ref(false)
const showTimePicker = ref(false)
const minDate = new Date()
const maxDate = new Date(minDate.getFullYear() + 1, 11, 31)
const pickerDate = ref<string[]>([])
const pickerTime = ref<string[]>(['10', '00'])
type PickerTarget = 'start' | 'end' | 'rescheduleStart' | 'rescheduleEnd'
let pickerTarget: PickerTarget = 'start'

function pad(n: number) {
  return String(n).padStart(2, '0')
}

function splitDateTime(value: string): { date: string[]; time: string[] } {
  const d = new Date(value.replace(' ', 'T'))
  if (Number.isNaN(d.getTime())) {
    const now = new Date()
    return { date: [String(now.getFullYear()), pad(now.getMonth() + 1), pad(now.getDate())], time: ['10', '00'] }
  }
  return {
    date: [String(d.getFullYear()), pad(d.getMonth() + 1), pad(d.getDate())],
    time: [pad(d.getHours()), pad(d.getMinutes())],
  }
}

function openPicker(target: 'start' | 'end') {
  pickerTarget = target
  const current = target === 'start' ? form.value.start_time : form.value.end_time
  const parts = splitDateTime(current)
  pickerDate.value = parts.date
  pickerTime.value = parts.time
  showDatePicker.value = true
}

function onDateConfirm({ selectedValues }: { selectedValues: string[] }) {
  pickerDate.value = selectedValues
  showDatePicker.value = false
  showTimePicker.value = true
}

function onTimeConfirm({ selectedValues }: { selectedValues: string[] }) {
  pickerTime.value = selectedValues
  showTimePicker.value = false
  const value = `${pickerDate.value.join('-')} ${pickerTime.value.join(':')}:00`
  if (pickerTarget === 'start' || pickerTarget === 'end') {
    form.value[pickerTarget === 'start' ? 'start_time' : 'end_time'] = value
  } else {
    rescheduleForm.value[pickerTarget === 'rescheduleStart' ? 'start_time' : 'end_time'] = value
  }
}

/* ---------- 改约 ---------- */

const showReschedule = ref(false)
const rescheduleId = ref(0)
const rescheduleForm = ref({ station_id: '', start_time: '', end_time: '', remark: '' })

function openReschedule(r: Reservation) {
  rescheduleId.value = r.id
  rescheduleForm.value = {
    station_id: String(r.station_id),
    start_time: formatTime(r.start_time),
    end_time: formatTime(r.end_time),
    remark: r.remark || '',
  }
  showReschedule.value = true
}

function openReschedulePicker(target: 'rescheduleStart' | 'rescheduleEnd') {
  pickerTarget = target
  const current = target === 'rescheduleStart' ? rescheduleForm.value.start_time : rescheduleForm.value.end_time
  const parts = splitDateTime(current)
  pickerDate.value = parts.date
  pickerTime.value = parts.time
  showDatePicker.value = true
}

async function submitReschedule() {
  if (!rescheduleForm.value.start_time || !rescheduleForm.value.end_time) {
    showToast('请选择新的起止时间')
    return
  }
  const stationId = Number(rescheduleForm.value.station_id)
  await rescheduleReservation(rescheduleId.value, {
    station_id: stationId || undefined,
    start_time: rescheduleForm.value.start_time,
    end_time: rescheduleForm.value.end_time,
    remark: rescheduleForm.value.remark,
  })
  showSuccessToast('改约成功')
  showReschedule.value = false
  load()
}

// van-dialog before-close：点确认时先提交，成功才关闭；取消直接关闭。
async function onRescheduleClose(action: string) {
  if (action !== 'confirm') return true
  try {
    await submitReschedule()
    return true
  } catch {
    return false
  }
}

onMounted(load)
</script>

<style scoped>
.submit-btn { margin: 12px 16px; }
.op-group { display: flex; align-items: center; flex-wrap: wrap; justify-content: flex-end; gap: 4px; }
.op-btn { margin-left: 6px; }
.reschedule-tip { padding: 8px 16px 16px; color: #969799; font-size: 12px; line-height: 1.5; }
</style>
