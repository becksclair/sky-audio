from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message

DESCRIPTOR: _descriptor.FileDescriptor

class AudioChunk(_message.Message):
    pcm_data: bytes
    is_end_of_speech: bool
    sample_rate: int
    def __init__(self, *, pcm_data: bytes = ..., is_end_of_speech: bool = ..., sample_rate: int = ...) -> None: ...

class Transcript(_message.Message):
    text: str
    is_final: bool
    def __init__(self, *, text: str = ..., is_final: bool = ...) -> None: ...

class TextToken(_message.Message):
    text: str
    is_eos: bool
    def __init__(self, *, text: str = ..., is_eos: bool = ...) -> None: ...

class WorkerStatus(_message.Message):
    vram_usage_mb: float
    is_busy: bool
    loaded_model: str
    def __init__(self, *, vram_usage_mb: float = ..., is_busy: bool = ..., loaded_model: str = ...) -> None: ...

class Empty(_message.Message):
    def __init__(self) -> None: ...
