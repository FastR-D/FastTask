/** Minimal structural Web Bluetooth types; no global navigator augmentation. */
export interface PanelBluetoothCharacteristic extends EventTarget {
  readonly value?: DataView;
  readonly properties: { write: boolean; notify: boolean; indicate: boolean };
  readValue(): Promise<DataView>;
  writeValueWithResponse(value: BufferSource): Promise<void>;
  startNotifications(): Promise<PanelBluetoothCharacteristic>;
  stopNotifications(): Promise<PanelBluetoothCharacteristic>;
}

export interface PanelBluetoothService {
  getCharacteristic(uuid: string): Promise<PanelBluetoothCharacteristic>;
}

export interface PanelBluetoothServer {
  readonly connected: boolean;
  connect(): Promise<PanelBluetoothServer>;
  disconnect(): void;
  getPrimaryService(uuid: string): Promise<PanelBluetoothService>;
}

export interface PanelBluetoothDevice extends EventTarget {
  readonly id: string;
  readonly name?: string;
  readonly gatt?: PanelBluetoothServer;
}

export interface PanelBluetooth {
  requestDevice(options: {
    filters?: Array<{ name?: string; namePrefix?: string; services?: string[] }>;
    acceptAllDevices?: boolean;
    optionalServices: string[];
  }): Promise<PanelBluetoothDevice>;
}
